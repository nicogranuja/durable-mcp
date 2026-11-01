package github_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicogranuja/durable-mcp/internal/github"
)

func issueJSON(n int) string {
	return fmt.Sprintf(`{"id":%d,"number":%d,"title":"Issue %d","state":"open","user":{"id":1,"login":"octocat","type":"User"},`+
		`"labels":[{"name":"bug"}],"comments":2,"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z","closed_at":null}`,
		1000+n, n, n)
}

func pullJSON(n int) string {
	return fmt.Sprintf(`{"id":%d,"number":%d,"title":"PR %d","state":"open",`+
		`"pull_request":{"url":"https://api.github.com/repos/acme/widgets/pulls/%d","merged_at":null}}`,
		1000+n, n, n, n)
}

// issuePages serves pages[i] as page i+1 of /repos/acme/widgets/issues.
// Like GitHub, its next links point at /repositories/42/issues rather than
// the path the client first requested. It records every request URL.
type issuePages struct {
	pages [][]string

	mu       sync.Mutex
	requests []*url.URL
}

func (s *issuePages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.URL)
	s.mu.Unlock()

	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	if page < 1 || page > len(s.pages) {
		http.NotFound(w, r)
		return
	}
	if page < len(s.pages) {
		next := fmt.Sprintf("http://%s/repositories/42/issues?per_page=%s&page=%d", r.Host, r.URL.Query().Get("per_page"), page+1)
		last := fmt.Sprintf("http://%s/repositories/42/issues?per_page=%s&page=%d", r.Host, r.URL.Query().Get("per_page"), len(s.pages))
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next", <%s>; rel="last"`, next, last))
	}
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(5000-page))
	w.Header().Set("X-RateLimit-Used", strconv.Itoa(page))
	w.Header().Set("X-RateLimit-Reset", "1793568638")
	w.Header().Set("X-RateLimit-Resource", "core")
	fmt.Fprint(w, "["+strings.Join(s.pages[page-1], ",")+"]")
}

func (s *issuePages) requestURLs() []*url.URL {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// threePages has an issue and a PR, two issues, and a page with only a PR.
func threePages() *issuePages {
	return &issuePages{pages: [][]string{
		{issueJSON(1), pullJSON(2)},
		{issueJSON(3), issueJSON(4)},
		{pullJSON(5)},
	}}
}

func numbers(issues []github.Issue) []int {
	ns := make([]int, len(issues))
	for i, is := range issues {
		ns[i] = is.Number
	}
	return ns
}

func TestListIssuesAllPages(t *testing.T) {
	t.Parallel()

	srv := threePages()
	c := newTestClient(t, srv.ServeHTTP)
	since := time.Date(2026, 10, 1, 9, 30, 0, 0, time.FixedZone("PDT", -7*60*60))

	pager, err := c.ListIssues("acme", "widgets", github.IssueListOptions{
		State:       github.StateAll,
		Since:       since,
		ListOptions: github.ListOptions{PerPage: 2},
	})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}

	var got []github.Issue
	for issue, err := range pager.All(t.Context()) {
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		got = append(got, issue)
	}

	if want := []int{1, 3, 4}; !slices.Equal(numbers(got), want) {
		t.Errorf("issue numbers = %v, want %v (pull requests excluded)", numbers(got), want)
	}
	first := got[0]
	if first.Title != "Issue 1" || first.User.Login != "octocat" || len(first.Labels) != 1 || first.ClosedAt != nil {
		t.Errorf("decoded issue = %+v", first)
	}

	reqs := srv.requestURLs()
	if len(reqs) != 3 {
		t.Fatalf("server received %d requests, want 3", len(reqs))
	}
	wantQuery := url.Values{"state": {"all"}, "since": {"2026-10-01T16:30:00Z"}, "per_page": {"2"}}
	if reqs[0].Path != "/repos/acme/widgets/issues" || reqs[0].Query().Encode() != wantQuery.Encode() {
		t.Errorf("first request = %s, want /repos/acme/widgets/issues?%s", reqs[0], wantQuery.Encode())
	}
	for _, r := range reqs[1:] {
		if r.Path != "/repositories/42/issues" {
			t.Errorf("follow-up request path = %s, want the Link header's /repositories/42/issues", r.Path)
		}
	}
}

func TestPagerNextPage(t *testing.T) {
	t.Parallel()

	srv := threePages()
	c := newTestClient(t, srv.ServeHTTP)
	pager, err := c.ListIssues("acme", "widgets", github.IssueListOptions{ListOptions: github.ListOptions{PerPage: 2}})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}

	if !pager.HasNext() || !strings.Contains(pager.Cursor(), "/repos/acme/widgets/issues") {
		t.Fatalf("before the first page: HasNext = %t, Cursor = %q", pager.HasNext(), pager.Cursor())
	}
	if pager.Rate() != (github.Rate{}) {
		t.Errorf("Rate before the first page = %+v, want zero", pager.Rate())
	}

	steps := []struct {
		wantNumbers []int
		wantHasNext bool
		wantCursor  string // substring; "" means the cursor must be empty
	}{
		{wantNumbers: []int{1}, wantHasNext: true, wantCursor: "/repositories/42/issues?per_page=2&page=2"},
		{wantNumbers: []int{3, 4}, wantHasNext: true, wantCursor: "page=3"},
		// The last page holds only a pull request: no items, but not an error.
		{wantNumbers: []int{}, wantHasNext: false, wantCursor: ""},
	}
	for i, step := range steps {
		issues, err := pager.NextPage(t.Context())
		if err != nil {
			t.Fatalf("page %d: NextPage: %v", i+1, err)
		}
		if !slices.Equal(numbers(issues), step.wantNumbers) {
			t.Errorf("page %d: numbers = %v, want %v", i+1, numbers(issues), step.wantNumbers)
		}
		if pager.HasNext() != step.wantHasNext {
			t.Errorf("page %d: HasNext = %t, want %t", i+1, pager.HasNext(), step.wantHasNext)
		}
		cursor := pager.Cursor()
		if (step.wantCursor == "") != (cursor == "") || !strings.Contains(cursor, step.wantCursor) {
			t.Errorf("page %d: Cursor = %q, want it to contain %q", i+1, cursor, step.wantCursor)
		}
		if got := pager.Rate(); got.Remaining != 5000-(i+1) || got.Resource != "core" {
			t.Errorf("page %d: Rate = %+v", i+1, got)
		}
	}

	issues, err := pager.NextPage(t.Context())
	if issues != nil || err != nil {
		t.Errorf("NextPage after the last page = %v, %v; want nil, nil", issues, err)
	}
	if n := len(srv.requestURLs()); n != 3 {
		t.Errorf("server received %d requests, want 3", n)
	}
}

func TestPagerPageLimit(t *testing.T) {
	t.Parallel()

	// A server whose Link header never ends.
	var hits atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		w.Header().Set("Link", fmt.Sprintf(`<http://%s/repositories/42/issues?page=%d>; rel="next"`, r.Host, n+1))
		fmt.Fprint(w, "["+issueJSON(int(n))+"]")
	}, github.WithMaxPages(3))

	pager, err := c.ListIssues("acme", "widgets", github.IssueListOptions{})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}

	var got []int
	var iterErr error
	for issue, err := range pager.All(t.Context()) {
		if err != nil {
			iterErr = err
			break
		}
		got = append(got, issue.Number)
	}

	if !errors.Is(iterErr, github.ErrPageLimit) {
		t.Fatalf("iteration error = %v, want ErrPageLimit", iterErr)
	}
	if want := []int{1, 2, 3}; !slices.Equal(got, want) {
		t.Errorf("issues before the limit = %v, want %v", got, want)
	}
	if hits.Load() != 3 {
		t.Errorf("server received %d requests, want 3", hits.Load())
	}
	if !pager.HasNext() || !strings.Contains(pager.Cursor(), "page=4") {
		t.Errorf("after the limit: HasNext = %t, Cursor = %q; want the cursor at page 4", pager.HasNext(), pager.Cursor())
	}
}

func TestPagerResumeFromCursor(t *testing.T) {
	t.Parallel()

	srv := threePages()
	c, ts := newTestServer(t, srv.ServeHTTP)

	first, err := c.ListIssues("acme", "widgets", github.IssueListOptions{ListOptions: github.ListOptions{PerPage: 2}})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if _, err := first.NextPage(t.Context()); err != nil {
		t.Fatalf("NextPage: %v", err)
	}
	cursor := first.Cursor()

	// A different client, as in a later Temporal activity in another
	// process, resumes from the cursor. The cursor already encodes the
	// query, so the other options must not change the request.
	other, err := github.New(testToken, github.WithBaseURL(ts.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resumed, err := other.ListIssues("acme", "widgets", github.IssueListOptions{
		State:       github.StateClosed,
		ListOptions: github.ListOptions{Cursor: cursor, PerPage: 50},
	})
	if err != nil {
		t.Fatalf("ListIssues with cursor: %v", err)
	}
	issues, err := resumed.NextPage(t.Context())
	if err != nil {
		t.Fatalf("NextPage: %v", err)
	}
	if want := []int{3, 4}; !slices.Equal(numbers(issues), want) {
		t.Errorf("resumed page = %v, want %v", numbers(issues), want)
	}

	reqs := srv.requestURLs()
	if got := reqs[len(reqs)-1]; ts.URL+got.String() != cursor {
		t.Errorf("resumed request = %s, want the cursor %s", ts.URL+got.String(), cursor)
	}
}

func TestPagerRetriesSamePageAfterError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fail    http.HandlerFunc
		wantErr error
	}{
		{
			name:    "server error",
			fail:    respond(http.StatusBadGateway, `{"message":"Server Error"}`),
			wantErr: github.ErrServer,
		},
		{
			name:    "primary rate limit",
			fail:    respond(http.StatusForbidden, primaryLimitBody, "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", "1793568638"),
			wantErr: github.ErrRateLimited,
		},
		{
			name:    "secondary rate limit",
			fail:    respond(http.StatusTooManyRequests, secondaryLimitBody, "Retry-After", "1"),
			wantErr: github.ErrRateLimited,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pages := threePages()
			var failed atomic.Bool
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				// Fail the first attempt at page 2 only.
				if r.URL.Query().Get("page") == "2" && failed.CompareAndSwap(false, true) {
					tt.fail(w, r)
					return
				}
				pages.ServeHTTP(w, r)
			})

			pager, err := c.ListIssues("acme", "widgets", github.IssueListOptions{ListOptions: github.ListOptions{PerPage: 2}})
			if err != nil {
				t.Fatalf("ListIssues: %v", err)
			}
			if _, err := pager.NextPage(t.Context()); err != nil {
				t.Fatalf("page 1: %v", err)
			}
			cursor := pager.Cursor()

			if _, err := pager.NextPage(t.Context()); !errors.Is(err, tt.wantErr) {
				t.Fatalf("page 2, first attempt: error = %v, want %v", err, tt.wantErr)
			}
			if pager.Cursor() != cursor || !pager.HasNext() {
				t.Fatalf("the pager advanced after an error: Cursor = %q, want %q", pager.Cursor(), cursor)
			}

			issues, err := pager.NextPage(t.Context())
			if err != nil {
				t.Fatalf("page 2, retry: %v", err)
			}
			if want := []int{3, 4}; !slices.Equal(numbers(issues), want) {
				t.Errorf("page 2 = %v, want %v", numbers(issues), want)
			}
		})
	}
}

func TestPagerNextLinkOrigin(t *testing.T) {
	t.Parallel()

	// foreign stands in for a host the token must never be sent to.
	var foreignHits atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignHits.Add(1)
		fmt.Fprint(w, "[]")
	}))
	t.Cleanup(foreign.Close)

	tests := []struct {
		name     string
		basePath string
		// link returns the Link header for the first page given the API
		// server's host.
		link    func(host string) string
		wantErr bool
	}{
		{
			name: "absolute link on the same origin",
			link: func(host string) string { return `<http://` + host + `/repositories/42/issues?page=2>; rel="next"` },
		},
		{
			name: "relative link",
			link: func(string) string { return `</repositories/42/issues?page=2>; rel="next"` },
		},
		{
			name:    "link to another host",
			link:    func(string) string { return `<` + foreign.URL + `/repositories/42/issues?page=2>; rel="next"` },
			wantErr: true,
		},
		{
			name:    "link to another scheme",
			link:    func(host string) string { return `<https://` + host + `/repositories/42/issues?page=2>; rel="next"` },
			wantErr: true,
		},
		{
			name:     "link outside the enterprise API path",
			basePath: "/api/v3",
			link:     func(host string) string { return `<http://` + host + `/admin/issues?page=2>; rel="next"` },
			wantErr:  true,
		},
		{
			name:     "dot segments escaping the enterprise API path",
			basePath: "/api/v3",
			link:     func(host string) string { return `<http://` + host + `/api/v3/../admin/issues?page=2>; rel="next"` },
			wantErr:  true,
		},
		{
			name:     "percent-encoded dot segments",
			basePath: "/api/v3",
			link:     func(string) string { return `</api/v3/%2e%2e/login/issues?page=2>; rel="next"` },
			wantErr:  true,
		},
		{
			name:     "dot segments with an encoded slash",
			basePath: "/api/v3",
			link:     func(string) string { return `</api/v3/..%2f..%2fadmin/issues?page=2>; rel="next"` },
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "" {
					w.Header().Set("Link", tt.link(r.Host))
				}
				fmt.Fprint(w, "["+issueJSON(1)+"]")
			}))
			t.Cleanup(srv.Close)
			c, err := github.New(testToken, github.WithBaseURL(srv.URL+tt.basePath))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			pager, err := c.ListIssues("acme", "widgets", github.IssueListOptions{})
			if err != nil {
				t.Fatalf("ListIssues: %v", err)
			}
			_, err = pager.NextPage(t.Context())
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "points outside") {
					t.Errorf("NextPage error = %v, want a refusal to follow the link", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NextPage: %v", err)
			}
			if !strings.HasPrefix(pager.Cursor(), srv.URL+"/repositories/42/issues") {
				t.Errorf("Cursor = %q, want the resolved next link", pager.Cursor())
			}
		})
	}

	t.Cleanup(func() {
		if n := foreignHits.Load(); n != 0 {
			t.Errorf("foreign host received %d requests, want 0", n)
		}
	})
}

func TestListIssuesInvalidCursor(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "[]")
	}))
	t.Cleanup(srv.Close)
	c, err := github.New(testToken, github.WithBaseURL(srv.URL+"/api/v3"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name   string
		cursor string
	}{
		{name: "another host", cursor: "https://attacker.example/api/v3/repositories/42/issues?page=2"},
		{name: "relative URL", cursor: "/api/v3/repositories/42/issues?page=2"},
		{name: "outside the API path", cursor: srv.URL + "/repositories/42/issues?page=2"},
		{name: "dot segments", cursor: srv.URL + "/api/v3/../repositories/42/issues?page=2"},
		{name: "percent-encoded dot segments", cursor: srv.URL + "/api/v3/%2e%2e/%2e%2e/admin/issues"},
		{name: "another resource", cursor: srv.URL + "/api/v3/repositories/42/pulls?page=2"},
		{name: "credentials", cursor: strings.Replace(srv.URL, "http://", "http://user:pass@", 1) + "/api/v3/repositories/42/issues"},
		{name: "unparseable", cursor: "http://%zz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := c.ListIssues("acme", "widgets", github.IssueListOptions{ListOptions: github.ListOptions{Cursor: tt.cursor}})
			if !errors.Is(err, github.ErrInvalidCursor) {
				t.Errorf("ListIssues error = %v, want ErrInvalidCursor", err)
			}
		})
	}

	t.Cleanup(func() {
		if n := hits.Load(); n != 0 {
			t.Errorf("server received %d requests, want 0", n)
		}
	})
}

func TestListOptionsPerPage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		perPage int
		want    string // expected per_page query value; "" means an error
	}{
		{perPage: 0, want: "100"},
		{perPage: 1, want: "1"},
		{perPage: 100, want: "100"},
		{perPage: 101},
		{perPage: -1},
	}

	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.perPage), func(t *testing.T) {
			t.Parallel()
			var got atomic.Value
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				got.Store(r.URL.Query().Get("per_page"))
				fmt.Fprint(w, "[]")
			})

			pager, err := c.ListIssues("acme", "widgets", github.IssueListOptions{ListOptions: github.ListOptions{PerPage: tt.perPage}})
			if tt.want == "" {
				if !errors.Is(err, github.ErrInvalidArgument) {
					t.Fatalf("ListIssues error = %v, want ErrInvalidArgument", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListIssues: %v", err)
			}
			if _, err := pager.NextPage(t.Context()); err != nil {
				t.Fatalf("NextPage: %v", err)
			}
			if got.Load() != tt.want {
				t.Errorf("per_page = %v, want %s", got.Load(), tt.want)
			}
		})
	}
}

func TestAllStopsFetchingWhenLoopBreaks(t *testing.T) {
	t.Parallel()

	srv := threePages()
	c := newTestClient(t, srv.ServeHTTP)
	pager, err := c.ListIssues("acme", "widgets", github.IssueListOptions{ListOptions: github.ListOptions{PerPage: 2}})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}

	for _, err := range pager.All(t.Context()) {
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		break
	}

	if n := len(srv.requestURLs()); n != 1 {
		t.Errorf("server received %d requests, want 1", n)
	}
	if !strings.Contains(pager.Cursor(), "page=2") {
		t.Errorf("Cursor = %q, want the pager positioned at page 2", pager.Cursor())
	}
}
