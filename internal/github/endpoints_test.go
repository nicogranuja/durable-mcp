package github_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/nicogranuja/durable-mcp/internal/github"
)

// recorder answers every request with body and remembers the last URL.
type recorder struct {
	body string

	mu   sync.Mutex
	last *url.URL
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.last = req.URL
	r.mu.Unlock()
	fmt.Fprint(w, r.body)
}

func (r *recorder) lastURL() *url.URL {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

const pullsJSON = `[
	{"id": 11, "number": 7, "title": "Add retries", "state": "closed", "draft": false,
	 "user": {"id": 1, "login": "octocat", "type": "User"}, "labels": [{"name": "enhancement"}],
	 "html_url": "https://github.com/acme/widgets/pull/7",
	 "created_at": "2026-10-01T10:00:00Z", "updated_at": "2026-10-03T10:00:00Z",
	 "closed_at": "2026-10-03T10:00:00Z", "merged_at": "2026-10-03T10:00:00Z"},
	{"id": 12, "number": 8, "title": "WIP: metrics", "state": "open", "draft": true,
	 "user": {"id": 2, "login": "dependabot[bot]", "type": "Bot"}, "labels": [],
	 "created_at": "2026-10-02T10:00:00Z", "updated_at": "2026-10-02T10:00:00Z",
	 "closed_at": null, "merged_at": null}
]`

const commitsJSON = `[
	{"sha": "6dcb09b5b57875f334f61aebed695e2e4193db5e", "html_url": "https://github.com/acme/widgets/commit/6dcb09b",
	 "commit": {"message": "Fix all the bugs",
	            "author": {"name": "Mona Octocat", "email": "mona@example.com", "date": "2026-10-05T08:00:00Z"},
	            "committer": {"name": "GitHub", "email": "noreply@github.com", "date": "2026-10-05T09:00:00Z"}},
	 "author": {"id": 3, "login": "mona", "type": "User"}},
	{"sha": "aa218f56b14c9653891f9e74264a383fa43fefbd",
	 "commit": {"message": "Initial commit",
	            "author": {"name": "Someone", "email": "someone@example.com", "date": "2026-09-01T08:00:00Z"},
	            "committer": {"name": "Someone", "email": "someone@example.com", "date": "2026-09-01T08:00:00Z"}},
	 "author": null}
]`

func TestListPullRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		opts      github.PullRequestListOptions
		wantQuery string
	}{
		{name: "defaults", wantQuery: "per_page=100"},
		{
			name: "state, sort, and direction",
			opts: github.PullRequestListOptions{
				State:       github.StateAll,
				Sort:        "updated",
				Direction:   "desc",
				ListOptions: github.ListOptions{PerPage: 30},
			},
			wantQuery: "direction=desc&per_page=30&sort=updated&state=all",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{body: pullsJSON}
			c := newTestClient(t, rec.ServeHTTP)

			pager, err := c.ListPullRequests("acme", "widgets", tt.opts)
			if err != nil {
				t.Fatalf("ListPullRequests: %v", err)
			}
			prs, err := pager.NextPage(t.Context())
			if err != nil {
				t.Fatalf("NextPage: %v", err)
			}

			if u := rec.lastURL(); u.Path != "/repos/acme/widgets/pulls" || u.Query().Encode() != tt.wantQuery {
				t.Errorf("request = %s, want /repos/acme/widgets/pulls?%s", u, tt.wantQuery)
			}
			if len(prs) != 2 {
				t.Fatalf("got %d pull requests, want 2", len(prs))
			}
			merged, draft := prs[0], prs[1]
			wantMerged := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
			if merged.Number != 7 || merged.MergedAt == nil || !merged.MergedAt.Equal(wantMerged) || merged.Labels[0].Name != "enhancement" {
				t.Errorf("merged pull request = %+v", merged)
			}
			if !draft.Draft || draft.MergedAt != nil || draft.ClosedAt != nil || draft.User.Type != "Bot" {
				t.Errorf("draft pull request = %+v", draft)
			}
		})
	}
}

func TestListCommits(t *testing.T) {
	t.Parallel()

	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 10, 1, 0, 0, 0, 0, time.FixedZone("CET", 60*60))

	tests := []struct {
		name      string
		opts      github.CommitListOptions
		wantQuery string
	}{
		{name: "defaults", wantQuery: "per_page=100"},
		{
			name:      "branch and time window",
			opts:      github.CommitListOptions{SHA: "release-1.2", Since: since, Until: until},
			wantQuery: "per_page=100&sha=release-1.2&since=2026-09-01T00%3A00%3A00Z&until=2026-09-30T23%3A00%3A00Z",
		},
		{
			name:      "since only",
			opts:      github.CommitListOptions{Since: since, ListOptions: github.ListOptions{PerPage: 10}},
			wantQuery: "per_page=10&since=2026-09-01T00%3A00%3A00Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{body: commitsJSON}
			c := newTestClient(t, rec.ServeHTTP)

			pager, err := c.ListCommits("acme", "widgets", tt.opts)
			if err != nil {
				t.Fatalf("ListCommits: %v", err)
			}
			commits, err := pager.NextPage(t.Context())
			if err != nil {
				t.Fatalf("NextPage: %v", err)
			}

			if u := rec.lastURL(); u.Path != "/repos/acme/widgets/commits" || u.RawQuery != tt.wantQuery {
				t.Errorf("request = %s, want /repos/acme/widgets/commits?%s", u, tt.wantQuery)
			}
			if len(commits) != 2 {
				t.Fatalf("got %d commits, want 2", len(commits))
			}
			linked, unlinked := commits[0], commits[1]
			wantAuthored := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
			if linked.Author == nil || linked.Author.Login != "mona" || linked.Commit.Message != "Fix all the bugs" ||
				linked.Commit.Author.Name != "Mona Octocat" || !linked.Commit.Author.Date.Equal(wantAuthored) {
				t.Errorf("linked commit = %+v", linked)
			}
			if unlinked.Author != nil {
				t.Errorf("commit without a matching account has Author = %+v, want nil", unlinked.Author)
			}
		})
	}
}

func TestListCommitsRejectsInvertedWindow(t *testing.T) {
	t.Parallel()

	c, err := github.New(testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now()
	_, err = c.ListCommits("acme", "widgets", github.CommitListOptions{Since: now, Until: now.Add(-time.Hour)})
	if !errors.Is(err, github.ErrInvalidArgument) {
		t.Errorf("ListCommits error = %v, want ErrInvalidArgument", err)
	}
}

func TestListMethodsValidateRepo(t *testing.T) {
	t.Parallel()

	// No server: validation must fail before any request is attempted.
	c, err := github.New(testToken, github.WithBaseURL("http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name string
		call func(owner, repo string) error
	}{
		{"ListIssues", func(o, r string) error {
			_, err := c.ListIssues(o, r, github.IssueListOptions{})
			return err
		}},
		{"ListPullRequests", func(o, r string) error {
			_, err := c.ListPullRequests(o, r, github.PullRequestListOptions{})
			return err
		}},
		{"ListCommits", func(o, r string) error {
			_, err := c.ListCommits(o, r, github.CommitListOptions{})
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, args := range [][2]string{{"", "widgets"}, {"acme", ""}, {"acme", "../../user"}} {
				if err := tt.call(args[0], args[1]); !errors.Is(err, github.ErrInvalidArgument) {
					t.Errorf("%s(%q, %q) error = %v, want ErrInvalidArgument", tt.name, args[0], args[1], err)
				}
			}
		})
	}
}
