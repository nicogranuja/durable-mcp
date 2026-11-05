package report_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"

	"github.com/nicogranuja/durable-mcp/internal/github"
	"github.com/nicogranuja/durable-mcp/internal/report"
)

// window is the test report window: October 2026, which starts on a
// Thursday and ends on a Saturday, so it touches five ISO weeks.
var window = report.Window{
	Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	Until: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
}

func day(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }

// before is a time shortly before the window.
func before() time.Time { return window.Since.Add(-time.Hour) }

// obj is a JSON object in a fixture.
type obj = map[string]any

// list encodes items as the JSON array a list endpoint returns.
func list(items ...obj) string {
	b, err := json.Marshal(items)
	if err != nil {
		panic(err)
	}
	return string(b)
}

const repoJSON = `{
	"id": 1296269, "name": "widgets", "full_name": "acme/widgets",
	"owner": {"id": 7, "login": "acme", "type": "Organization"},
	"description": "Widgets for everyone", "html_url": "https://github.com/acme/widgets",
	"default_branch": "main", "archived": false, "stargazers_count": 42, "forks_count": 3
}`

// newActivities starts an httptest server with handler and returns report
// activities whose GitHub client talks to it.
func newActivities(t *testing.T, handler http.Handler) *report.Activities {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	gh, err := github.New("test-token", github.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	return report.NewActivities(gh)
}

// respond returns a handler that writes a fixed status, headers, and body.
func respond(status int, body string, headerKV ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headerKV); i += 2 {
			w.Header().Set(headerKV[i], headerKV[i+1])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

// paged serves pages[i] as page i+1 of a listing, with a Link header to the
// next page, the way GitHub paginates. It records every request's query.
type paged struct {
	pages []string

	mu      sync.Mutex
	queries []url.Values
}

func (p *paged) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p.mu.Lock()
	p.queries = append(p.queries, q)
	p.mu.Unlock()

	page := 1
	if s := q.Get("page"); s != "" {
		page, _ = strconv.Atoi(s)
	}
	if page < 1 || page > len(p.pages) {
		http.NotFound(w, r)
		return
	}
	if page < len(p.pages) {
		nq := r.URL.Query()
		nq.Set("page", strconv.Itoa(page+1))
		next := url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path, RawQuery: nq.Encode()}
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next.String()))
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, p.pages[page-1])
}

func (p *paged) requests() []url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.queries)
}

// testLogger returns a Temporal logger that keeps the SDK's debug output and
// prints it only if the test fails.
func testLogger(t *testing.T) log.Logger {
	t.Helper()
	buf := &lockedBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Temporal SDK log:\n%s", buf.String())
		}
	})
	return log.NewStructuredLogger(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

// lockedBuffer is a bytes.Buffer that is safe for concurrent use; the SDK
// logs from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newActivityEnv returns a Temporal test environment that runs acts.
func newActivityEnv(t *testing.T, acts *report.Activities) *testsuite.TestActivityEnvironment {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	s.SetLogger(testLogger(t))
	env := s.NewTestActivityEnvironment()
	env.RegisterActivity(acts)
	return env
}

// execute runs an activity in env and decodes its result into an R.
func execute[R any](t *testing.T, env *testsuite.TestActivityEnvironment, activity, arg any) R {
	t.Helper()
	val, err := env.ExecuteActivity(activity, arg)
	if err != nil {
		t.Fatalf("activity failed: %v", err)
	}
	var r R
	if err := val.Get(&r); err != nil {
		t.Fatalf("decode activity result: %v", err)
	}
	return r
}
