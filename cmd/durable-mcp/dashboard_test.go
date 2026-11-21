package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/nicogranuja/durable-mcp/internal/auth"
	"github.com/nicogranuja/durable-mcp/internal/github"
	"github.com/nicogranuja/durable-mcp/internal/mcpserver"
	"github.com/nicogranuja/durable-mcp/internal/report"
	"github.com/nicogranuja/durable-mcp/internal/telemetry"
)

const dashboardPath = "../../deploy/grafana/durable-mcp.json"

// TestDashboardMetricsAreEmitted keeps the Grafana dashboard in step with
// the code. It runs the real code paths into a Prometheus registry, through
// the same exporter -metrics-addr serves: tool calls through serve's HTTP
// handler (one rejected with 401), and a report workflow whose real
// activities call a fake GitHub API that rate-limits once. Then it checks
// that every metric each dashboard query selects is in the scrape, with
// every label the query matches or groups by.
func TestDashboardMetricsAreEmitted(t *testing.T) {
	t.Parallel()

	scrape := emitAllMetrics(t)
	exprs := dashboardExprs(t)
	if len(exprs) == 0 {
		t.Fatal("the dashboard has no queries")
	}
	for _, e := range exprs {
		for _, problem := range checkExpr(e.expr, scrape) {
			t.Errorf("panel %q: %s\nexpr: %s", e.panel, problem, e.expr)
		}
	}
}

// The check itself must catch a metric or label the code does not emit:
// otherwise the test above would pass on any dashboard.
func TestCheckExprFindsMissingMetrics(t *testing.T) {
	t.Parallel()

	scrape := parseScrape(`# TYPE jobs_total counter
jobs_total{outcome="completed",otel_scope_name="x"} 1
`)
	tests := []struct {
		expr string
		want int
	}{
		{`sum by (outcome) (rate(jobs_total[$__rate_interval]))`, 0},
		{`sum(rate(jobs_total{outcome="failed"}[5m])) / sum(rate(jobs_total[5m]))`, 0},
		{`sum(rate(job_total[5m]))`, 1},                        // misspelled metric
		{`sum by (kind) (rate(jobs_total[5m]))`, 1},            // label it does not have
		{`rate(jobs_total{error_type!=""}[5m])`, 1},            // the same, in a matcher
		{`histogram_quantile(0.95, rate(jobs_bucket[5m]))`, 1}, // series that do not exist
	}
	for _, tt := range tests {
		if got := checkExpr(tt.expr, scrape); len(got) != tt.want {
			t.Errorf("checkExpr(%q) = %q, want %d problems", tt.expr, got, tt.want)
		}
	}
}

// emitAllMetrics records every metric the dashboard can show and returns
// the Prometheus scrape.
func emitAllMetrics(t *testing.T) scrapeIndex {
	t.Helper()
	tel, err := telemetry.New(t.Context(), telemetry.Config{Component: "test", Prometheus: true, Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}

	// serve: a started job, a tool error, and a rejected token.
	tokens, err := auth.ParseTokenList(auth.Entry("alice", aliceToken))
	if err != nil {
		t.Fatal(err)
	}
	server := mcpserver.New(startsJobs{}, mcpserver.Options{MeterProvider: tel.MeterProvider(), Logger: discardLogger()})
	srv := httptest.NewServer(httpHandler(server, tokens, tel, discardLogger()))
	t.Cleanup(srv.Close)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-host"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + mcpPath,
		HTTPClient: &http.Client{Transport: bearer{token: aliceToken}},
	}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "start_report", Arguments: map[string]any{"owner": "acme", "repo": "widgets"}})
	if err != nil || res.IsError {
		t.Fatalf("CallTool(start_report) = %+v, %v", res, err)
	}
	if res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "job_status", Arguments: map[string]any{"job_id": "not-a-job"}}); err != nil || !res.IsError {
		t.Fatalf("CallTool(job_status) = %+v, %v; want a tool error", res, err)
	}
	_ = cs.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+mcpPath, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer test-token-wrong")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status with a wrong token = %d, want 401", resp.StatusCode)
	}

	// worker: a report whose first issues page hits the primary rate limit,
	// and one on a repository that does not exist.
	api := httptest.NewServer(fakeGitHub())
	t.Cleanup(api.Close)
	gh, err := github.New("test-token-github-not-a-secret", github.WithBaseURL(api.URL+"/"),
		github.WithHTTPClient(&http.Client{Transport: tel.HTTPTransport(nil)}))
	if err != nil {
		t.Fatal(err)
	}
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(log.NewStructuredLogger(discardLogger()))
	suite.SetMetricsHandler(tel.TemporalMetricsHandler())
	runReport := func(repo string) error {
		wenv := suite.NewTestWorkflowEnvironment()
		wenv.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{report.NewMetricsInterceptor()}})
		report.Register(wenv, report.NewActivities(gh))
		wenv.ExecuteWorkflow(report.RepositoryReport, report.Request{Owner: "acme", Repo: repo})
		return wenv.GetWorkflowError()
	}
	if err := runReport("widgets"); err != nil {
		t.Fatalf("report on acme/widgets failed: %v", err)
	}
	// A failed job, for the outcome's error type.
	if err := runReport("nope"); !strings.Contains(fmt.Sprint(err), report.ErrorTypeNotFound) {
		t.Fatalf("report on acme/nope = %v, want a %s failure", err, report.ErrorTypeNotFound)
	}

	rec := httptest.NewRecorder()
	tel.PrometheusHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, metricsPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape = %d:\n%s", rec.Code, rec.Body)
	}
	return parseScrape(rec.Body.String())
}

// startsJobs is a Temporal client whose starts succeed.
type startsJobs struct{ mcpserver.Client }

func (startsJobs) ExecuteWorkflow(context.Context, client.StartWorkflowOptions, any, ...any) (client.WorkflowRun, error) {
	return nil, nil
}

// fakeGitHub serves acme/widgets, one page of each listing, and answers
// the first issues request with the primary rate limit. Any other
// repository is not found.
func fakeGitHub() http.Handler {
	day := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	reply := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		}
	}
	var limited atomic.Bool
	mux := http.NewServeMux()
	mux.Handle("GET /repos/acme/widgets", reply(http.StatusOK,
		`{"id":1,"name":"widgets","full_name":"acme/widgets","owner":{"login":"acme"},"default_branch":"main"}`))
	mux.HandleFunc("GET /repos/acme/widgets/issues", func(w http.ResponseWriter, r *http.Request) {
		if limited.CompareAndSwap(false, true) {
			w.Header().Set("X-RateLimit-Limit", "60")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
			reply(http.StatusForbidden, `{"message":"API rate limit exceeded for 127.0.0.1."}`)(w, r)
			return
		}
		reply(http.StatusOK, `[{"number":1,"state":"open","created_at":"`+day+`","updated_at":"`+day+`"}]`)(w, r)
	})
	mux.Handle("GET /repos/acme/widgets/pulls", reply(http.StatusOK, `[]`))
	mux.Handle("GET /repos/acme/widgets/commits", reply(http.StatusOK, `[]`))
	mux.Handle("/", reply(http.StatusNotFound, `{"message":"Not Found"}`))
	return mux
}

type dashboardExpr struct{ panel, expr string }

// dashboardExprs returns the query of every target of every panel, rows'
// nested panels included.
func dashboardExprs(t *testing.T) []dashboardExpr {
	t.Helper()
	b, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatal(err)
	}
	type panel struct {
		Title   string  `json:"title"`
		Panels  []panel `json:"panels"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	}
	var d struct {
		Panels []panel `json:"panels"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("parse %s: %v", dashboardPath, err)
	}
	var out []dashboardExpr
	var walk func([]panel)
	walk = func(ps []panel) {
		for _, p := range ps {
			for _, tg := range p.Targets {
				out = append(out, dashboardExpr{p.Title, tg.Expr})
			}
			walk(p.Panels)
		}
	}
	walk(d.Panels)
	return out
}

// scrapeIndex maps each sample name in a scrape (such as
// http_client_request_duration_seconds_bucket) to its label names.
type scrapeIndex map[string]map[string]bool

var (
	sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})?\s`)
	labelName  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="(?:[^"\\]|\\.)*"`)
)

// parseScrape indexes a scrape in the Prometheus text format.
func parseScrape(body string) scrapeIndex {
	idx := scrapeIndex{}
	for line := range strings.Lines(body) {
		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if idx[m[1]] == nil {
			idx[m[1]] = map[string]bool{}
		}
		for _, l := range labelName.FindAllStringSubmatch(m[2], -1) {
			idx[m[1]][l[1]] = true
		}
	}
	return idx
}

var (
	promString   = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	promMatchers = regexp.MustCompile(`\{([^}]*)\}`)
	promMatcher  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(?:=~|!~|!=|=)`)
	promGrouping = regexp.MustCompile(`\b(?:by|without|on|ignoring|group_left|group_right)\s*\(([^)]*)\)`)
	promRange    = regexp.MustCompile(`\[[^\]]*\]`)
	promIdent    = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)
	promKeywords = []string{"and", "or", "unless", "bool", "offset", "inf", "nan"}
)

// checkExpr returns what in a PromQL expression the scrape lacks: each
// metric it selects that has no samples, and each label it matches on or
// groups by that none of its metrics has. It is a small lexer, not a
// PromQL parser, enough for the dashboard's queries: an identifier is a
// metric unless it is a function (followed by a parenthesis), a keyword,
// or inside a grouping clause, a label matcher, a string, or a range.
func checkExpr(expr string, scrape scrapeIndex) []string {
	var labels []string
	s := promString.ReplaceAllString(expr, `""`)
	for _, m := range promMatchers.FindAllStringSubmatch(s, -1) {
		for _, l := range promMatcher.FindAllStringSubmatch(m[1], -1) {
			labels = append(labels, l[1])
		}
	}
	s = promMatchers.ReplaceAllString(s, " ")
	for _, m := range promGrouping.FindAllStringSubmatch(s, -1) {
		for l := range strings.SplitSeq(m[1], ",") {
			if l = strings.TrimSpace(l); l != "" {
				labels = append(labels, l)
			}
		}
	}
	s = promGrouping.ReplaceAllString(s, " ")
	s = promRange.ReplaceAllString(s, " ")

	var problems, metrics []string
	for _, loc := range promIdent.FindAllStringIndex(s, -1) {
		name := s[loc[0]:loc[1]]
		rest := strings.TrimLeft(s[loc[1]:], " \t\n")
		if strings.HasPrefix(rest, "(") || slices.Contains(promKeywords, strings.ToLower(name)) ||
			(loc[0] > 0 && (s[loc[0]-1] >= '0' && s[loc[0]-1] <= '9' || s[loc[0]-1] == '.')) {
			continue // a function, a keyword, or the exponent of a number
		}
		if _, ok := scrape[name]; !ok {
			problems = append(problems, fmt.Sprintf("metric %s is not emitted", name))
			continue
		}
		metrics = append(metrics, name)
	}
	for _, l := range labels {
		has := slices.ContainsFunc(metrics, func(m string) bool { return scrape[m][l] })
		if !has && len(metrics) > 0 {
			problems = append(problems, fmt.Sprintf("label %s is on none of %v", l, metrics))
		}
	}
	return problems
}
