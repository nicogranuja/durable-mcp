// Package github is a small client for the parts of the GitHub REST API that
// the repository report needs: repositories, issues, pull requests, and
// commits.
//
// It is written directly on net/http rather than on a generated SDK so that
// the behaviour that matters for long-running jobs is explicit and testable:
// authentication headers, per-request timeouts, response size limits,
// Link-header pagination, and rate-limit handling.
//
// # Errors
//
// Every non-2xx response is returned as either a [*RateLimitError] or an
// [*APIError]. Classify errors with [errors.Is] against the sentinels
// ([ErrUnauthorized], [ErrForbidden], [ErrNotFound], [ErrValidation],
// [ErrServer], [ErrRateLimited]) and use [errors.As] to read the details.
// Transport failures, including context cancellation and timeouts, are
// wrapped so that errors.Is(err, context.Canceled) and
// errors.Is(err, context.DeadlineExceeded) work as expected.
//
// # Rate limits
//
// The client never sleeps and never retries. When GitHub rejects a request
// because of its primary or secondary rate limit, the client returns a
// [*RateLimitError] whose RetryAt field says when a retry can succeed, and
// leaves the decision to the caller.
//
// This is deliberate. A primary rate-limit reset can be up to an hour away.
// Sleeping that long inside an HTTP call hides the wait from the caller and
// ties up a goroutine for the whole period; in this project it would also
// hold a Temporal activity slot open and force the activity to keep
// heartbeating while doing nothing. Returning a typed error lets the caller
// apply its own policy. In this project the caller is a Temporal activity,
// which is meant to fail the attempt with a retry delay derived from RetryAt,
// so the wait happens durably in Temporal and shows up in workflow history.
package github
