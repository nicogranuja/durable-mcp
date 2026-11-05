// Package report builds a report of a GitHub repository's issue, pull
// request, and commit activity over a time window, as a Temporal workflow.
//
// [RepositoryReport] is the workflow and [Activities] are its activities;
// [Register] registers both with a worker.
//
// # Workflow and activities
//
// The activities do all the I/O. Each call makes one GitHub request: the
// repository, or one page of issues, pull requests, or commits. The next
// page's cursor, from the GitHub client's Pager.Cursor, travels in one
// activity's result and the next one's input. A failed attempt therefore
// repeats one page, never the pages before it, and a worker that crashes
// mid-report loses at most the page it was fetching.
//
// The workflow decides what to fetch next and adds up the results. It has
// no I/O, clock, or randomness of its own: the default window ends at the
// run's start time, as recorded in its history, and the code that combines
// pages never ranges over a map. A replay therefore makes the same
// decisions.
//
// Resources are fetched one after another rather than in parallel: GitHub
// counts concurrent requests from one token against its secondary rate
// limit, and a report is not latency sensitive. Concurrent reports share
// the worker's token, so the worker command also caps how many activities
// it runs at once.
//
// # History size
//
// An activity does not return the items it fetched. It reduces its page to
// a small partial aggregate (counts, merge durations, per-week commit
// counts, and commits per author), and the workflow adds the partials up.
// Activity inputs and results are stored in workflow history, so the
// history grows with the number of pages, not with the size of the issues
// and pull requests on them: each page adds six events (the activity's
// scheduled, started, and completed events and the workflow task that
// follows) and at most a few kilobytes of payload. Retries add no events,
// because the server records only an activity's final attempt.
//
// Request.MaxPages bounds the pages per resource, at most MaxPagesLimit, so
// a report has at most about 3,600 events and 2 MB of history. That is below
// the 4,096 events at which the Temporal server suggests continue-as-new by
// default, and far below the limits at which it warns (10,240 events) and
// terminates a run (51,200), so the workflow does not need continue-as-new.
// A resource with more pages is reported as truncated.
//
// Adding partials is commutative and the final report sorts everything it
// lists, so the result does not depend on the order pages arrive in.
//
// # Retries and timeouts
//
// Activities classify the GitHub client's errors into application errors
// with stable types (ErrorTypeRateLimited and the other ErrorType
// constants):
//
//   - A rate limit is retryable, with NextRetryDelay set from the time
//     GitHub said the limit resets, so the next attempt runs then and not on
//     the retry policy's schedule. The wait happens on the Temporal server:
//     no worker slot or goroutine is held, it survives worker restarts, and
//     the pending activity shows the attempt, the rate-limit failure, and
//     when the next attempt is due. Secondary rate limits wait at least a
//     backoff that doubles with each attempt at the page, as GitHub asks.
//   - 401, 403, 404, 422, and other 4xx responses except 408, invalid
//     arguments, and oversized pages are non-retryable: the same request
//     cannot succeed, so the report fails at once with a type that says
//     why.
//   - 5xx responses and network errors are retried with the retry policy's
//     exponential backoff.
//
// NextRetryDelay needs Temporal Server 1.24.2 or later. An older server
// ignores it and retries a rate-limited page on the retry policy's backoff
// instead, which wastes requests while the limit lasts.
//
// Every activity uses the same options, chosen for an activity that makes
// one request:
//
//   - StartToCloseTimeout, 1 minute, bounds one attempt. It is longer than
//     the client's 30-second request timeout, so a slow request fails with
//     the client's more specific error first.
//   - HeartbeatTimeout, 10 seconds, detects a lost worker much sooner than
//     the start-to-close timeout would, and bounds how long a cancellation
//     takes to reach a running activity.
//   - The retry policy starts at 1 second and doubles up to 1 minute, for
//     at most 10 attempts: a page that keeps failing with server errors
//     fails the report after about four minutes instead of retrying
//     forever. Rate-limit retries use NextRetryDelay instead of this backoff
//     but count as attempts.
//   - ScheduleToCloseTimeout, 2 hours, is a page's total budget, retries
//     included. It must outlast the longest normal rate-limit wait, a
//     primary reset at most an hour away, with room to spare. A reset beyond
//     the budget is not waited for: the server fails the activity with the
//     rate-limit error instead.
//
// # Heartbeats and cancellation
//
// Activities heartbeat with their resource and page while their request is
// in flight. That is what lets Temporal notice a lost worker within the
// heartbeat timeout, and it is how a cancellation reliably reaches the
// activity: Temporal reports it in a heartbeat response (newer servers can
// also push it to the worker), and the SDK then cancels the activity's
// context, which aborts the request.
//
// Canceling the workflow cancels the activity in flight and ends the run as
// canceled. The workflow does not wait for the activity to acknowledge the
// cancellation: the report has nothing to clean up, and an activity of a
// closed workflow is canceled at its next heartbeat anyway.
//
// # Progress
//
// The [ProgressQuery] query returns the current [Phase], the pages fetched
// per resource, and the error that ended the run if it failed. A retry in
// progress, such as a page waiting for a rate limit, is not workflow state;
// it is in the pending activity that DescribeWorkflowExecution returns.
//
// # Limitations
//
// GitHub paginates these listings by offset. If issues or pull requests
// change while a report runs, which is likelier when it waits out a rate
// limit, items can shift between pages and be counted twice or missed. The
// counts are a close estimate for an active repository, not an exact
// snapshot.
package report
