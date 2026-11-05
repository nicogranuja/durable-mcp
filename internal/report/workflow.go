package report

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// DefaultTaskQueue is the task queue that reports run on unless the worker
// and the client that starts reports agree on another.
const DefaultTaskQueue = "durable-mcp"

// ProgressQuery is the name of the workflow query that returns a report's
// Progress.
const ProgressQuery = "progress"

// Register registers the RepositoryReport workflow and the report
// activities with a worker.
func Register(r worker.Registry, acts *Activities) {
	r.RegisterWorkflow(RepositoryReport)
	r.RegisterActivity(acts)
}

// Phase is the stage a report is in.
type Phase string

// Phases of a report, in order. A report ends in PhaseCompleted,
// PhaseFailed, or PhaseCanceled.
const (
	PhaseStarted      Phase = "started"
	PhaseRepository   Phase = "repository"
	PhaseIssues       Phase = "issues"
	PhasePullRequests Phase = "pull_requests"
	PhaseCommits      Phase = "commits"
	PhaseCompleted    Phase = "completed"
	PhaseFailed       Phase = "failed"
	PhaseCanceled     Phase = "canceled"
)

// Progress is the answer to ProgressQuery.
type Progress struct {
	Phase Phase `json:"phase"`

	// Pages counts the pages fetched so far, per resource.
	Pages PageCounts `json:"pages"`

	// Error describes the failure that ended the report, once Phase is
	// PhaseFailed.
	//
	// It does not show failed attempts that Temporal is still retrying,
	// such as a page waiting for a rate limit to reset: activity retries
	// happen on the Temporal server and the workflow sees only the final
	// outcome. Those are in the pending activity that
	// DescribeWorkflowExecution returns, with its attempt number, last
	// failure, and next attempt time.
	Error string `json:"error,omitempty"`
}

// PageCounts counts pages per resource.
type PageCounts struct {
	Issues       int `json:"issues"`
	PullRequests int `json:"pull_requests"`
	Commits      int `json:"commits"`
}

// finish records how the report ended. It must stay free of side effects
// such as logging: it is deferred, so it also runs, with a nil error, when
// the SDK evicts the workflow from its cache.
func (p *Progress) finish(err error) {
	switch {
	case err == nil:
		p.Phase = PhaseCompleted
	case temporal.IsCanceledError(err):
		p.Phase = PhaseCanceled
	default:
		p.Phase = PhaseFailed
		p.Error = failureMessage(err)
	}
}

// failureMessage describes the error that ended a report: the application
// error or timeout that caused it, which names its type, and why Temporal
// stopped retrying, rather than the activity error wrapped around them.
func failureMessage(err error) string {
	msg := err.Error()
	if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
		msg = appErr.Error()
	} else if timeoutErr, ok := errors.AsType[*temporal.TimeoutError](err); ok {
		msg = timeoutErr.Error()
	}
	if actErr, ok := errors.AsType[*temporal.ActivityError](err); ok {
		switch actErr.RetryState() {
		case enumspb.RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED:
			msg += "; gave up after the maximum number of attempts"
		case enumspb.RETRY_STATE_TIMEOUT:
			msg += "; gave up when the schedule-to-close timeout ran out"
		}
	}
	return msg
}

// activityOptions returns the options for every report activity. Each
// activity makes one GitHub request; see the package documentation for how
// these values follow from that.
func activityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    time.Minute,
		HeartbeatTimeout:       10 * time.Second,
		ScheduleToCloseTimeout: 2 * time.Hour,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    10,
		},
	}
}

// RepositoryReport is a workflow that reports a repository's issue, pull
// request, and commit activity over a time window.
//
// It fetches the repository, then every page of issues, pull requests, and
// commits in turn, one activity per page, and adds up the pages' partial
// aggregates as they arrive. Resources are fetched one after another, not
// in parallel, because GitHub counts concurrent requests from one token
// against its secondary rate limit. Reports running at the same time share
// the worker's token, so the worker also caps its concurrent activities.
//
// The ProgressQuery query reports how far it has got. Canceling the
// workflow cancels the activity in flight and ends the report in
// PhaseCanceled.
func RepositoryReport(ctx workflow.Context, req Request) (_ Report, err error) {
	progress := Progress{Phase: PhaseStarted}
	err = workflow.SetQueryHandlerWithOptions(ctx, ProgressQuery,
		func() (Progress, error) { return progress, nil },
		workflow.QueryHandlerOptions{Description: "Phase and pages fetched so far"})
	if err != nil {
		return Report{}, err
	}
	defer func() { progress.finish(err) }()

	// The start time, not workflow.Now: the run may start before any worker
	// picks it up, and the report covers the time it was requested.
	window, maxPages, err := req.resolve(workflow.GetInfo(ctx).WorkflowStartTime)
	if err != nil {
		return Report{}, temporal.NewNonRetryableApplicationError(err.Error(), ErrorTypeInvalidRequest, nil)
	}
	ctx = workflow.WithActivityOptions(ctx, activityOptions())
	var a *Activities // only names the activities; the worker's instance runs them

	progress.Phase = PhaseRepository
	r := Report{Window: window}
	repoCtx := withSummary(ctx, req.Owner+"/"+req.Repo)
	err = workflow.ExecuteActivity(repoCtx, a.FetchRepository, RepositoryRequest{Owner: req.Owner, Repo: req.Repo}).
		Get(repoCtx, &r.Repository)
	if err != nil {
		return Report{}, err
	}

	first := PageRequest{Owner: req.Owner, Repo: req.Repo, Window: window}

	progress.Phase = PhaseIssues
	r.Issues.Truncated, err = fetchAll(ctx, resourceIssues, a.FetchIssuesPage, first, maxPages, func(s IssueStats) {
		r.Issues.add(s)
		progress.Pages.Issues++
	})
	if err != nil {
		return Report{}, err
	}

	progress.Phase = PhasePullRequests
	var prs PullRequestStats
	truncated, err := fetchAll(ctx, resourcePullRequests, a.FetchPullRequestsPage, first, maxPages, func(s PullRequestStats) {
		prs.add(s)
		progress.Pages.PullRequests++
	})
	if err != nil {
		return Report{}, err
	}
	r.PullRequests = prs.report(truncated)

	progress.Phase = PhaseCommits
	var commits CommitStats
	truncated, err = fetchAll(ctx, resourceCommits, a.FetchCommitsPage, first, maxPages, func(s CommitStats) {
		commits.add(s)
		progress.Pages.Commits++
	})
	if err != nil {
		return Report{}, err
	}
	r.Commits = commits.report(window, truncated)

	return r, nil
}

// fetchAll runs activity once per page of a resource, starting at the
// first page and following each page's Next cursor, and passes each page's
// partial aggregate to add. It stops after the last page or after maxPages
// pages, and reports whether it stopped with pages left.
//
// Pages are fetched one at a time because each page's cursor comes from the
// page before it.
func fetchAll[S any](
	ctx workflow.Context,
	resource string,
	activity func(context.Context, PageRequest) (Page[S], error),
	req PageRequest,
	maxPages int,
	add func(S),
) (truncated bool, err error) {
	for n := 1; ; n++ {
		if n > maxPages {
			return true, nil
		}
		req.Page = n
		pageCtx := withSummary(ctx, fmt.Sprintf("%s page %d", resource, n))
		var page Page[S]
		if err := workflow.ExecuteActivity(pageCtx, activity, req).Get(pageCtx, &page); err != nil {
			return false, err
		}
		add(page.Stats)
		if page.Next == "" {
			return false, nil
		}
		req.Cursor = page.Next
	}
}

// withSummary sets the one-line summary that the Temporal UI and CLI show
// for the next activity.
func withSummary(ctx workflow.Context, summary string) workflow.Context {
	opts := workflow.GetActivityOptions(ctx)
	opts.Summary = summary
	return workflow.WithActivityOptions(ctx, opts)
}
