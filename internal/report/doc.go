// Package report builds a GitHub repository report: issue, pull request, and
// commit activity over a time window.
//
// # Activities
//
// [Activities] fetch data from GitHub one page per activity call. The next
// page's cursor, from the GitHub client's Pager.Cursor, travels in the activity's
// result and the next activity's input, so a failed attempt repeats one
// page, never the pages before it, and a worker that crashes mid-report
// loses at most the page it was fetching.
//
// Activities heartbeat while their request is in flight. That lets
// Temporal notice a lost worker within the heartbeat timeout rather than the
// longer start-to-close timeout, and it is how a cancellation reaches
// the activity: Temporal delivers it in a heartbeat response, and the SDK
// then cancels the activity's context, which aborts the request.
//
// # Aggregation
//
// An activity does not return the items it fetched. It reduces its page to
// a small partial aggregate (counts, merge durations, per-week commit
// counts, and commits per author), and the workflow adds the partials up.
// Activity inputs and results are stored in workflow history, so this keeps
// the history proportional to the number of pages rather than the size of
// the repository's issues and pull requests. Adding partials is commutative
// and the final report sorts everything it lists, so the result does not
// depend on the order pages are combined in.
package report
