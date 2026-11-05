package report_test

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

func pageRequest(cursor string, page int) report.PageRequest {
	return report.PageRequest{Owner: "acme", Repo: "widgets", Window: window, Cursor: cursor, Page: page}
}

func TestFetchRepository(t *testing.T) {
	t.Parallel()

	acts := newActivities(t, respond(http.StatusOK, repoJSON))
	env := newActivityEnv(t, acts)

	got := execute[report.Repository](t, env, acts.FetchRepository, report.RepositoryRequest{Owner: "acme", Repo: "widgets"})
	want := report.Repository{
		FullName:      "acme/widgets",
		Description:   "Widgets for everyone",
		HTMLURL:       "https://github.com/acme/widgets",
		DefaultBranch: "main",
		Stars:         42,
		Forks:         3,
	}
	if got != want {
		t.Errorf("FetchRepository() = %+v, want %+v", got, want)
	}
}

func TestFetchIssuesPage(t *testing.T) {
	t.Parallel()

	pages := &paged{pages: []string{
		list(
			obj{"number": 1, "state": "open", "created_at": day(2), "updated_at": day(2)},
			// The issues endpoint also returns pull requests; they are not issues.
			obj{"number": 2, "state": "open", "created_at": day(2), "updated_at": day(2), "pull_request": obj{}},
			obj{"number": 3, "state": "closed", "state_reason": "completed", "created_at": before(), "updated_at": day(3), "closed_at": day(3)},
		),
		list(obj{"number": 4, "state": "closed", "state_reason": "not_planned", "created_at": day(5), "updated_at": day(6), "closed_at": day(6)}),
	}}
	acts := newActivities(t, pages)
	env := newActivityEnv(t, acts)

	first := execute[report.Page[report.IssueStats]](t, env, acts.FetchIssuesPage, pageRequest("", 1))
	if want := (report.IssueStats{Opened: 1, Closed: 1, ClosedAs: report.CloseReasons{Completed: 1}, StillOpen: 1}); first.Stats != want {
		t.Errorf("page 1 stats = %+v, want %+v", first.Stats, want)
	}
	if first.Next == "" {
		t.Fatal("page 1 Next is empty, want a cursor for page 2")
	}

	// A later activity resumes from the cursor, as the workflow does.
	second := execute[report.Page[report.IssueStats]](t, env, acts.FetchIssuesPage, pageRequest(first.Next, 2))
	if want := (report.IssueStats{Opened: 1, Closed: 1, ClosedAs: report.CloseReasons{NotPlanned: 1}}); second.Stats != want {
		t.Errorf("page 2 stats = %+v, want %+v", second.Stats, want)
	}
	if second.Next != "" {
		t.Errorf("page 2 Next = %q, want empty on the last page", second.Next)
	}

	reqs := pages.requests()
	if len(reqs) != 2 {
		t.Fatalf("server got %d requests, want 2 (one per activity)", len(reqs))
	}
	if got, want := reqs[0].Encode(), "per_page=100&since=2026-10-01T00%3A00%3A00Z&state=all"; got != want {
		t.Errorf("first request query = %s, want %s", got, want)
	}
	if got := reqs[1].Get("page"); got != "2" {
		t.Errorf("second request page = %q, want 2", got)
	}
}

func TestFetchPullRequestsPageStopsBeforeTheWindow(t *testing.T) {
	t.Parallel()

	pages := &paged{pages: []string{
		list(
			obj{"number": 9, "state": "open", "created_at": day(20), "updated_at": day(21)},
			obj{"number": 8, "state": "closed", "created_at": day(10), "updated_at": day(11), "merged_at": day(11), "closed_at": day(11)},
			// Last updated before the window: it and everything after it,
			// on this page and the next, is older still.
			obj{"number": 7, "state": "closed", "created_at": before(), "updated_at": before(), "merged_at": before(), "closed_at": before()},
		),
		list(obj{"number": 6, "state": "open", "created_at": before(), "updated_at": before()}),
	}}
	acts := newActivities(t, pages)
	env := newActivityEnv(t, acts)

	got := execute[report.Page[report.PullRequestStats]](t, env, acts.FetchPullRequestsPage, pageRequest("", 1))
	want := report.Page[report.PullRequestStats]{Stats: report.PullRequestStats{
		Opened: 2, Merged: 1, StillOpen: 1, MergeSeconds: []int64{24 * 3600},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FetchPullRequestsPage() = %+v, want %+v", got, want)
	}

	reqs := pages.requests()
	if len(reqs) != 1 {
		t.Fatalf("server got %d requests, want 1", len(reqs))
	}
	if got, want := reqs[0].Encode(), "direction=desc&per_page=100&sort=updated&state=all"; got != want {
		t.Errorf("request query = %s, want %s", got, want)
	}
}

func TestFetchCommitsPage(t *testing.T) {
	t.Parallel()

	commit := func(date time.Time, login string) obj {
		c := obj{"sha": "abc", "commit": obj{"committer": obj{"date": date}, "author": obj{"date": date}}}
		if login != "" {
			c["author"] = obj{"login": login}
		}
		return c
	}
	pages := &paged{pages: []string{
		list(commit(day(30), "mona"), commit(day(14), "hubot"), commit(day(13), "mona"), commit(day(2), "")),
	}}
	acts := newActivities(t, pages)
	env := newActivityEnv(t, acts)

	got := execute[report.Page[report.CommitStats]](t, env, acts.FetchCommitsPage, pageRequest("", 1))
	want := report.Page[report.CommitStats]{Stats: report.CommitStats{
		Total:        4,
		Weekly:       []int{1, 0, 2, 0, 1},
		Authors:      []report.AuthorCount{{Login: "hubot", Commits: 1}, {Login: "mona", Commits: 2}},
		Unattributed: 1,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FetchCommitsPage() = %+v, want %+v", got, want)
	}
	if got, want := pages.requests()[0].Encode(), "per_page=100&since=2026-10-01T00%3A00%3A00Z&until=2026-11-01T00%3A00%3A00Z"; got != want {
		t.Errorf("request query = %s, want %s", got, want)
	}
}

func TestFetchCommitsPageOfEmptyRepository(t *testing.T) {
	t.Parallel()

	acts := newActivities(t, respond(http.StatusConflict, `{"message":"Git Repository is empty."}`))
	env := newActivityEnv(t, acts)

	got := execute[report.Page[report.CommitStats]](t, env, acts.FetchCommitsPage, pageRequest("", 1))
	if !reflect.DeepEqual(got, report.Page[report.CommitStats]{}) {
		t.Errorf("FetchCommitsPage() = %+v, want an empty last page", got)
	}
}

func TestActivitiesHeartbeatProgress(t *testing.T) {
	t.Parallel()

	acts := newActivities(t, &paged{pages: []string{list()}})
	env := newActivityEnv(t, acts)

	type details struct {
		Resource string `json:"resource"`
		Page     int    `json:"page"`
	}
	var (
		mu  sync.Mutex
		got []details
	)
	env.SetOnActivityHeartbeatListener(func(_ *activity.Info, v converter.EncodedValues) {
		var d details
		if err := v.Get(&d); err != nil {
			t.Errorf("decode heartbeat details: %v", err)
		}
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
	})

	execute[report.Page[report.IssueStats]](t, env, acts.FetchIssuesPage, pageRequest("", 3))

	mu.Lock()
	defer mu.Unlock()
	if want := (details{Resource: "issues", Page: 3}); len(got) == 0 || got[0] != want {
		t.Errorf("heartbeats = %+v, want the first to be %+v", got, want)
	}
}

// The SDK cancels an activity's context when Temporal cancels the activity,
// with a *temporal.CanceledError cause, and when the worker shuts down. The
// request in flight must stop then, rather than run to its own timeout, and
// only Temporal's cancellation may be reported as one.
func TestActivityStopsWhenItsContextIsCanceled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		cause        error
		wantCanceled bool
	}{
		{name: "canceled by Temporal", cause: temporal.NewCanceledError(), wantCanceled: true},
		{name: "worker shutting down", cause: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			started := make(chan struct{})
			var once sync.Once
			acts := newActivities(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				once.Do(func() { close(started) })
				select {
				case <-r.Context().Done():
				case <-time.After(10 * time.Second):
				}
			}))
			env := newActivityEnv(t, acts)
			// The activity's context derives from this one, so canceling it
			// with a cause stands in for the SDK canceling the activity.
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			env.SetWorkerOptions(worker.Options{BackgroundActivityContext: ctx})

			go func() {
				<-started
				cancel(tt.cause)
			}()
			begin := time.Now()
			_, err := env.ExecuteActivity(acts.FetchIssuesPage, pageRequest("", 1))
			if elapsed := time.Since(begin); elapsed > 5*time.Second {
				t.Errorf("activity took %s to stop after its context was canceled", elapsed)
			}
			if err == nil {
				t.Fatal("ExecuteActivity() succeeded, want it to stop with an error")
			}
			if got := temporal.IsCanceledError(err); got != tt.wantCanceled {
				t.Errorf("ExecuteActivity() error = %v; reported as canceled = %v, want %v", err, got, tt.wantCanceled)
			}
		})
	}
}
