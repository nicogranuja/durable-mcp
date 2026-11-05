package report

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/nicogranuja/durable-mcp/internal/github"
)

// Resource names, used in heartbeat details and logs.
const (
	resourceRepository   = "repository"
	resourceIssues       = "issues"
	resourcePullRequests = "pull_requests"
	resourceCommits      = "commits"
)

// Activities fetches report data from GitHub. Each method is an activity
// that makes exactly one request, so a retry repeats one page and never the
// pages before it. Register them with Register.
type Activities struct {
	github *github.Client
}

// NewActivities returns the report activities, which make their requests
// with gh.
func NewActivities(gh *github.Client) *Activities {
	return &Activities{github: gh}
}

// RepositoryRequest is the input of FetchRepository.
type RepositoryRequest struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// PageRequest is the input of the page activities.
type PageRequest struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Window Window `json:"window"`

	// Cursor is the page to fetch, from the previous page's Next. It is
	// empty for the first page.
	Cursor string `json:"cursor,omitempty"`

	// Page is the 1-based page number. It only labels heartbeats and logs.
	Page int `json:"page"`
}

// Page is the result of a page activity: the page's partial aggregate,
// rather than its items, and where the next page starts.
type Page[S any] struct {
	Stats S `json:"stats"`

	// Next is the cursor of the next page, or empty if the report needs no
	// more pages of this resource.
	Next string `json:"next,omitempty"`
}

// FetchRepository fetches the repository's description. The workflow calls
// it first, so a missing repository or a bad token fails the report after a
// single request.
func (a *Activities) FetchRepository(ctx context.Context, req RepositoryRequest) (Repository, error) {
	r, err := withHeartbeat(ctx, heartbeat{Resource: resourceRepository}, func() (*github.Repository, error) {
		return a.github.GetRepository(ctx, req.Owner, req.Repo)
	})
	if err != nil {
		return Repository{}, err
	}
	return Repository{
		FullName:      r.FullName,
		Description:   r.Description,
		HTMLURL:       r.HTMLURL,
		DefaultBranch: r.DefaultBranch,
		Stars:         r.StargazersCount,
		Forks:         r.ForksCount,
		Archived:      r.Archived,
	}, nil
}

// FetchIssuesPage fetches one page of the issues updated since the start of
// the window, which includes every issue opened or closed in it.
func (a *Activities) FetchIssuesPage(ctx context.Context, req PageRequest) (Page[IssueStats], error) {
	pager, err := a.github.ListIssues(req.Owner, req.Repo, github.IssueListOptions{
		State:       github.StateAll,
		Since:       req.Window.Since,
		ListOptions: github.ListOptions{Cursor: req.Cursor},
	})
	if err != nil {
		return Page[IssueStats]{}, err
	}
	issues, err := fetchPage(ctx, pager, resourceIssues, req.Page)
	if err != nil {
		return Page[IssueStats]{}, err
	}
	return Page[IssueStats]{Stats: pageIssueStats(issues, req.Window), Next: pager.Cursor()}, nil
}

// FetchPullRequestsPage fetches one page of pull requests, most recently
// updated first. GitHub cannot filter pull requests by date, so the listing
// ends at the first pull request last updated before the window, and Next is
// empty from that page on.
func (a *Activities) FetchPullRequestsPage(ctx context.Context, req PageRequest) (Page[PullRequestStats], error) {
	pager, err := a.github.ListPullRequests(req.Owner, req.Repo, github.PullRequestListOptions{
		State:       github.StateAll,
		Sort:        github.SortUpdated,
		Direction:   github.Descending,
		ListOptions: github.ListOptions{Cursor: req.Cursor},
	})
	if err != nil {
		return Page[PullRequestStats]{}, err
	}
	prs, err := fetchPage(ctx, pager, resourcePullRequests, req.Page)
	if err != nil {
		return Page[PullRequestStats]{}, err
	}
	stats, pastWindow := pagePullRequestStats(prs, req.Window)
	next := pager.Cursor()
	if pastWindow {
		next = ""
	}
	return Page[PullRequestStats]{Stats: stats, Next: next}, nil
}

// FetchCommitsPage fetches one page of the default branch's commits in the
// window.
func (a *Activities) FetchCommitsPage(ctx context.Context, req PageRequest) (Page[CommitStats], error) {
	pager, err := a.github.ListCommits(req.Owner, req.Repo, github.CommitListOptions{
		Since:       req.Window.Since,
		Until:       req.Window.Until,
		ListOptions: github.ListOptions{Cursor: req.Cursor},
	})
	if err != nil {
		return Page[CommitStats]{}, err
	}
	commits, err := fetchPage(ctx, pager, resourceCommits, req.Page)
	if apiErr, ok := errors.AsType[*github.APIError](err); ok && apiErr.StatusCode == http.StatusConflict {
		// GitHub answers 409 Conflict for a repository with no commits.
		return Page[CommitStats]{}, nil
	}
	if err != nil {
		return Page[CommitStats]{}, err
	}
	return Page[CommitStats]{Stats: pageCommitStats(commits, req.Window), Next: pager.Cursor()}, nil
}

// fetchPage fetches the pager's next page, heartbeating while the request
// is in flight.
func fetchPage[T any](ctx context.Context, p *github.Pager[T], resource string, page int) ([]T, error) {
	items, err := withHeartbeat(ctx, heartbeat{Resource: resource, Page: page}, func() ([]T, error) {
		return p.NextPage(ctx)
	})
	if err != nil {
		return nil, err
	}
	activity.GetLogger(ctx).Debug("Fetched page",
		"Resource", resource, "Page", page, "Items", len(items), "RateLimitRemaining", p.Rate().Remaining)
	return items, nil
}

// heartbeat is the progress an activity records in its heartbeats. It shows
// up in the pending activity in the Temporal UI and CLI.
type heartbeat struct {
	Resource string `json:"resource"`
	Page     int    `json:"page,omitempty"`
}

// defaultHeartbeatInterval is how often withHeartbeat records a heartbeat
// when the activity has no heartbeat timeout, as in some tests.
const defaultHeartbeatInterval = 10 * time.Second

// withHeartbeat calls fn and records heartbeats with details until it
// returns: one right away, then periodically.
//
// Heartbeating during the request, not only before it, matters for two
// reasons. The SDK sends at most one heartbeat per throttle interval (80% of
// the heartbeat timeout) and only if one was recorded since the last send,
// so a single heartbeat before a slow request would let the activity time
// out. And Temporal delivers a cancellation to an activity only in the
// response to a heartbeat; when it arrives, the SDK cancels ctx, which
// aborts the request.
func withHeartbeat[T any](ctx context.Context, details heartbeat, fn func() (T, error)) (T, error) {
	activity.RecordHeartbeat(ctx, details)

	// Recording is cheap: the SDK keeps only the latest details until the
	// throttle interval ends. Record several times per interval so none is
	// ever empty.
	interval := defaultHeartbeatInterval
	if timeout := activity.GetInfo(ctx).HeartbeatTimeout; timeout > 0 {
		interval = timeout / 4
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				activity.RecordHeartbeat(ctx, details)
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	})
	defer func() {
		close(done)
		wg.Wait()
	}()
	return fn()
}
