package report_test

import (
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// mocked names the activities in env.OnActivity, as the workflow does.
var mocked *report.Activities

// startTime is when test workflows start, on the test environment's clock.
var startTime = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func newWorkflowEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	s.SetLogger(testLogger(t))
	env := s.NewTestWorkflowEnvironment()
	env.SetStartTime(startTime)
	return env
}

var request = report.Request{Owner: "acme", Repo: "widgets", Since: window.Since, Until: window.Until}

func pageReq(cursor string, page int) report.PageRequest {
	return report.PageRequest{Owner: "acme", Repo: "widgets", Window: window, Cursor: cursor, Page: page}
}

var repository = report.Repository{FullName: "acme/widgets", HTMLURL: "https://github.com/acme/widgets", DefaultBranch: "main"}

// mockSmallRepository mocks the activities for a repository with two pages
// of issues and one page each of pull requests and commits. The pull
// requests page takes prDelay on the test clock.
func mockSmallRepository(env *testsuite.TestWorkflowEnvironment, prDelay time.Duration) {
	env.OnActivity(mocked.FetchRepository, mock.Anything, report.RepositoryRequest{Owner: "acme", Repo: "widgets"}).
		Return(repository, nil).Once()
	env.OnActivity(mocked.FetchIssuesPage, mock.Anything, pageReq("", 1)).
		Return(report.Page[report.IssueStats]{
			Stats: report.IssueStats{Opened: 3, Closed: 1, ClosedAs: report.CloseReasons{Completed: 1}, StillOpen: 2},
			Next:  "issues-2",
		}, nil).Once()
	env.OnActivity(mocked.FetchIssuesPage, mock.Anything, pageReq("issues-2", 2)).
		Return(report.Page[report.IssueStats]{
			Stats: report.IssueStats{Opened: 1, Closed: 2, ClosedAs: report.CloseReasons{NotPlanned: 1, Other: 1}},
		}, nil).Once()
	env.OnActivity(mocked.FetchPullRequestsPage, mock.Anything, pageReq("", 1)).
		After(prDelay).
		Return(report.Page[report.PullRequestStats]{
			Stats: report.PullRequestStats{Opened: 4, Merged: 3, ClosedUnmerged: 1, StillOpen: 1, MergeSeconds: []int64{3600, 60, 7200}},
		}, nil).Once()
	env.OnActivity(mocked.FetchCommitsPage, mock.Anything, pageReq("", 1)).
		Return(report.Page[report.CommitStats]{
			Stats: report.CommitStats{
				Total:        6,
				Weekly:       []int{1, 0, 4, 0, 1},
				Authors:      []report.AuthorCount{{Login: "hubot", Commits: 1}, {Login: "mona", Commits: 4}},
				Unattributed: 1,
			},
		}, nil).Once()
}

func TestRepositoryReport(t *testing.T) {
	t.Parallel()

	env := newWorkflowEnv(t)
	mockSmallRepository(env, 0)

	env.ExecuteWorkflow(report.RepositoryReport, request)

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}
	var got report.Report
	if err := env.GetWorkflowResult(&got); err != nil {
		t.Fatal(err)
	}
	want := report.Report{
		Repository: repository,
		Window:     window,
		Issues: report.IssueReport{IssueStats: report.IssueStats{
			Opened: 4, Closed: 3, ClosedAs: report.CloseReasons{Completed: 1, NotPlanned: 1, Other: 1}, StillOpen: 2,
		}},
		PullRequests: report.PullRequestReport{Opened: 4, Merged: 3, ClosedUnmerged: 1, StillOpen: 1, MedianTimeToMergeSeconds: 3600},
		Commits: report.CommitReport{
			Total: 6,
			Weekly: []report.WeekCount{
				{WeekOf: "2026-09-28", Commits: 1}, {WeekOf: "2026-10-05"}, {WeekOf: "2026-10-12", Commits: 4},
				{WeekOf: "2026-10-19"}, {WeekOf: "2026-10-26", Commits: 1},
			},
			TopContributors: []report.AuthorCount{{Login: "mona", Commits: 4}, {Login: "hubot", Commits: 1}},
			Contributors:    2,
			Unattributed:    1,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("report =\n%+v\nwant\n%+v", got, want)
	}
	env.AssertExpectations(t)

	progress := queryProgress(t, env)
	if want := (report.Progress{Phase: report.PhaseCompleted, Pages: report.PageCounts{Issues: 2, PullRequests: 1, Commits: 1}}); progress != want {
		t.Errorf("progress = %+v, want %+v", progress, want)
	}
}

func queryProgress(t *testing.T, env *testsuite.TestWorkflowEnvironment) report.Progress {
	t.Helper()
	val, err := env.QueryWorkflow(report.ProgressQuery)
	if err != nil {
		t.Fatalf("query %s: %v", report.ProgressQuery, err)
	}
	var p report.Progress
	if err := val.Get(&p); err != nil {
		t.Fatalf("decode progress: %v", err)
	}
	return p
}

// The default window is the DefaultWindow before the run started.
func TestRepositoryReportDefaultWindow(t *testing.T) {
	t.Parallel()

	want := report.Window{Since: startTime.Add(-report.DefaultWindow), Until: startTime}
	inWindow := mock.MatchedBy(func(r report.PageRequest) bool {
		return r.Window.Since.Equal(want.Since) && r.Window.Until.Equal(want.Until)
	})

	env := newWorkflowEnv(t)
	env.OnActivity(mocked.FetchRepository, mock.Anything, mock.Anything).Return(repository, nil)
	env.OnActivity(mocked.FetchIssuesPage, mock.Anything, inWindow).Return(report.Page[report.IssueStats]{}, nil).Once()
	env.OnActivity(mocked.FetchPullRequestsPage, mock.Anything, mock.Anything).Return(report.Page[report.PullRequestStats]{}, nil)
	env.OnActivity(mocked.FetchCommitsPage, mock.Anything, mock.Anything).Return(report.Page[report.CommitStats]{}, nil)

	env.ExecuteWorkflow(report.RepositoryReport, report.Request{Owner: "acme", Repo: "widgets"})

	var got report.Report
	if err := env.GetWorkflowResult(&got); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}
	if !got.Window.Since.Equal(want.Since) || !got.Window.Until.Equal(want.Until) {
		t.Errorf("report window = %v, want %v", got.Window, want)
	}
	env.AssertExpectations(t) // the activities saw the same window
	if n := len(got.Commits.Weekly); n != 5 {
		t.Errorf("report lists %d weeks, want 5, including weeks with no commits", n)
	}
}

func TestRepositoryReportStopsAtThePageLimit(t *testing.T) {
	t.Parallel()

	env := newWorkflowEnv(t)
	env.OnActivity(mocked.FetchRepository, mock.Anything, mock.Anything).Return(repository, nil)
	// Every resource claims there is always another page.
	env.OnActivity(mocked.FetchIssuesPage, mock.Anything, mock.Anything).
		Return(report.Page[report.IssueStats]{Stats: report.IssueStats{Opened: 1}, Next: "more"}, nil)
	env.OnActivity(mocked.FetchPullRequestsPage, mock.Anything, mock.Anything).
		Return(report.Page[report.PullRequestStats]{Stats: report.PullRequestStats{Opened: 1}, Next: "more"}, nil)
	env.OnActivity(mocked.FetchCommitsPage, mock.Anything, mock.Anything).
		Return(report.Page[report.CommitStats]{Stats: report.CommitStats{Total: 1}, Next: "more"}, nil)

	req := request
	req.MaxPages = 3
	env.ExecuteWorkflow(report.RepositoryReport, req)

	var got report.Report
	if err := env.GetWorkflowResult(&got); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}
	if !got.Issues.Truncated || !got.PullRequests.Truncated || !got.Commits.Truncated {
		t.Errorf("truncated = %v, %v, %v; want all true", got.Issues.Truncated, got.PullRequests.Truncated, got.Commits.Truncated)
	}
	if got.Issues.Opened != 3 || got.PullRequests.Opened != 3 || got.Commits.Total != 3 {
		t.Errorf("counts = %d, %d, %d; want 3 pages' worth each", got.Issues.Opened, got.PullRequests.Opened, got.Commits.Total)
	}
	env.AssertActivityNumberOfCalls(t, "FetchIssuesPage", 3)
	env.AssertActivityNumberOfCalls(t, "FetchCommitsPage", 3)
}

func TestRepositoryReportRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	env := newWorkflowEnv(t)
	report.Register(env, newActivities(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fakeRepository(issuesOK).ServeHTTP(w, r)
	})))

	env.ExecuteWorkflow(report.RepositoryReport, report.Request{Owner: "acme", Repo: "widgets", Since: window.Until, Until: window.Since})

	assertFailed(t, env, report.ErrorTypeInvalidRequest)
	if n := requests.Load(); n != 0 {
		t.Errorf("GitHub got %d requests, want none for an invalid request", n)
	}
}

// assertFailed checks that the workflow failed with a non-retryable
// application error of the given type, and that the progress query says
// so.
func assertFailed(t *testing.T, env *testsuite.TestWorkflowEnvironment, errType string) {
	t.Helper()
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	err := env.GetWorkflowError()
	appErr, ok := errors.AsType[*temporal.ApplicationError](err)
	if !ok || appErr.Type() != errType {
		t.Fatalf("workflow error = %v, want an application error of type %s", err, errType)
	}
	progress := queryProgress(t, env)
	if progress.Phase != report.PhaseFailed || !strings.Contains(progress.Error, errType) {
		t.Errorf("progress = %+v, want phase %s and an error naming %s", progress, report.PhaseFailed, errType)
	}
}

func TestRepositoryReportCanceled(t *testing.T) {
	t.Parallel()

	env := newWorkflowEnv(t)
	env.OnActivity(mocked.FetchRepository, mock.Anything, mock.Anything).Return(repository, nil)
	env.OnActivity(mocked.FetchIssuesPage, mock.Anything, pageReq("", 1)).
		Return(report.Page[report.IssueStats]{Next: "issues-2"}, nil).Once()
	// The second page is slow; the report is canceled while it is in flight.
	env.OnActivity(mocked.FetchIssuesPage, mock.Anything, pageReq("issues-2", 2)).
		After(time.Hour).Return(report.Page[report.IssueStats]{}, nil).Once()

	var canceled atomic.Bool
	env.SetOnActivityCanceledListener(func(*activity.Info) { canceled.Store(true) })
	env.RegisterDelayedCallback(env.CancelWorkflow, 10*time.Minute)
	env.ExecuteWorkflow(report.RepositoryReport, request)

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); !temporal.IsCanceledError(err) {
		t.Fatalf("workflow error = %v, want a cancellation", err)
	}
	if !canceled.Load() {
		t.Error("the activity in flight was not canceled")
	}
	if elapsed := env.Now().Sub(startTime); elapsed >= time.Hour {
		t.Errorf("workflow ran for %s; it should end when canceled, not wait for the activity", elapsed)
	}
	env.AssertActivityNumberOfCalls(t, "FetchPullRequestsPage", 0)
	env.AssertActivityNumberOfCalls(t, "FetchCommitsPage", 0)

	progress := queryProgress(t, env)
	if want := (report.Progress{Phase: report.PhaseCanceled, Pages: report.PageCounts{Issues: 1}}); progress != want {
		t.Errorf("progress = %+v, want %+v", progress, want)
	}
}

func TestRepositoryReportProgressWhileRunning(t *testing.T) {
	t.Parallel()

	env := newWorkflowEnv(t)
	// The pull requests page takes an hour; look in on the report half way.
	mockSmallRepository(env, time.Hour)

	var (
		during   report.Progress
		queryErr error
	)
	env.RegisterDelayedCallback(func() {
		var val converter.EncodedValue
		if val, queryErr = env.QueryWorkflow(report.ProgressQuery); queryErr == nil {
			queryErr = val.Get(&during)
		}
	}, 30*time.Minute)
	env.ExecuteWorkflow(report.RepositoryReport, request)

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}
	if queryErr != nil {
		t.Fatalf("query while running: %v", queryErr)
	}
	if want := (report.Progress{Phase: report.PhasePullRequests, Pages: report.PageCounts{Issues: 2}}); during != want {
		t.Errorf("progress while fetching pull requests = %+v, want %+v", during, want)
	}
}

// The remaining tests run the real activities against a fake GitHub API,
// so they cover the whole path from an HTTP response to a Temporal retry.

// sequence answers the i-th request with handlers[i], repeating the last
// handler once it runs out, and counts requests.
type sequence struct {
	handlers []http.HandlerFunc
	calls    atomic.Int32
}

func (s *sequence) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i := int(s.calls.Add(1)) - 1
	s.handlers[min(i, len(s.handlers)-1)](w, r)
}

// fakeRepository serves acme/widgets with one page of each resource, and
// issues from the given handler.
func fakeRepository(issues http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /repos/acme/widgets", respond(http.StatusOK, repoJSON))
	mux.Handle("GET /repos/acme/widgets/issues", issues)
	mux.Handle("GET /repos/acme/widgets/pulls", respond(http.StatusOK, list(
		obj{"number": 2, "state": "closed", "created_at": day(3), "updated_at": day(4), "merged_at": day(4), "closed_at": day(4)},
	)))
	mux.Handle("GET /repos/acme/widgets/commits", respond(http.StatusOK, list(
		obj{"sha": "abc", "commit": obj{"committer": obj{"date": day(4)}}, "author": obj{"login": "mona"}},
	)))
	return mux
}

var issuesOK = respond(http.StatusOK, list(obj{"number": 1, "state": "open", "created_at": day(2), "updated_at": day(2)}))

func TestRepositoryReportRetries(t *testing.T) {
	t.Parallel()

	rateLimited := func(resetIn time.Duration) http.HandlerFunc {
		reset := strconv.FormatInt(time.Now().Add(resetIn).Unix(), 10)
		return respond(http.StatusForbidden, primaryLimitBody,
			"X-RateLimit-Limit", "5000", "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset)
	}
	unavailable := respond(http.StatusBadGateway, `{"message":"Server Error"}`)

	tests := []struct {
		name      string
		responses []http.HandlerFunc
		wantCalls int32
		// wantErrType is empty if the report should succeed.
		wantErrType string
		// The workflow's run time on the test clock, which advances only
		// through timers and retry delays.
		wantMinElapsed, wantMaxElapsed time.Duration
	}{
		{
			name:           "rate limit retried at the reset",
			responses:      []http.HandlerFunc{rateLimited(30 * time.Minute), issuesOK},
			wantCalls:      2,
			wantMinElapsed: 29 * time.Minute, wantMaxElapsed: 31 * time.Minute,
		},
		{
			name:           "server errors retried with backoff",
			responses:      []http.HandlerFunc{unavailable, unavailable, issuesOK},
			wantCalls:      3,
			wantMinElapsed: 3 * time.Second, wantMaxElapsed: 4 * time.Second, // 1s, then 2s
		},
		{
			name:        "server errors give up after the maximum attempts",
			responses:   []http.HandlerFunc{unavailable},
			wantCalls:   10,
			wantErrType: report.ErrorTypeUnavailable,
			// 1+2+4+8+16+32 seconds, then three at the one-minute cap.
			wantMinElapsed: 243 * time.Second, wantMaxElapsed: 244 * time.Second,
		},
		{
			name: "rate limit beyond the schedule-to-close timeout is not retried",
			// The reset is further away than the page's whole retry budget,
			// so the server fails the activity instead of waiting.
			responses:   []http.HandlerFunc{rateLimited(3 * time.Hour), issuesOK},
			wantCalls:   1,
			wantErrType: report.ErrorTypeRateLimited,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issues := &sequence{handlers: tt.responses}
			env := newWorkflowEnv(t)
			report.Register(env, newActivities(t, fakeRepository(issues)))

			env.ExecuteWorkflow(report.RepositoryReport, request)

			if calls := issues.calls.Load(); calls != tt.wantCalls {
				t.Errorf("issues requests = %d, want %d", calls, tt.wantCalls)
			}
			if tt.wantErrType != "" {
				assertFailed(t, env, tt.wantErrType)
			} else {
				var got report.Report
				if err := env.GetWorkflowResult(&got); err != nil {
					t.Fatalf("workflow failed: %v", err)
				}
				if got.Issues.Opened != 1 || got.PullRequests.Merged != 1 || got.Commits.Total != 1 {
					t.Errorf("report = %+v, want one issue, merged pull request, and commit", got)
				}
			}
			if tt.wantMaxElapsed > 0 {
				elapsed := env.Now().Sub(startTime)
				if elapsed < tt.wantMinElapsed || elapsed > tt.wantMaxElapsed {
					t.Errorf("workflow took %s on the test clock, want %s to %s", elapsed, tt.wantMinElapsed, tt.wantMaxElapsed)
				}
			}
		})
	}
}

func TestRepositoryReportMissingRepository(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	env := newWorkflowEnv(t)
	report.Register(env, newActivities(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		respond(http.StatusNotFound, `{"message":"Not Found"}`)(w, r)
	})))

	env.ExecuteWorkflow(report.RepositoryReport, report.Request{Owner: "acme", Repo: "nope"})

	assertFailed(t, env, report.ErrorTypeNotFound)
	if n := requests.Load(); n != 1 {
		t.Errorf("GitHub got %d requests, want 1: a 404 is not retried and ends the report", n)
	}
}
