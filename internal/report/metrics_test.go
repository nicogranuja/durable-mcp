package report_test

import (
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/nicogranuja/durable-mcp/internal/report"
	"github.com/nicogranuja/durable-mcp/internal/telemetry"
	"github.com/nicogranuja/durable-mcp/internal/telemetry/telemetrytest"
)

// newMeteredEnv returns a test environment whose metrics, the workflow's,
// the activities', and the SDK's, go to the returned reader, through the
// same Temporal metrics handler and views as the worker's.
func newMeteredEnv(t *testing.T) (*testsuite.TestWorkflowEnvironment, *telemetrytest.Reader) {
	t.Helper()
	r := telemetrytest.New(t)
	var s testsuite.WorkflowTestSuite
	s.SetLogger(testLogger(t))
	s.SetMetricsHandler(telemetry.TemporalMetricsHandler(r.Provider, slog.New(slog.DiscardHandler)))
	env := s.NewTestWorkflowEnvironment()
	env.SetStartTime(startTime)
	env.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{report.NewMetricsInterceptor()}})
	return env, r
}

// assertBounded fails if any point of the metric has an attribute value
// that names the repository, as a job ID, repository, or caller label
// would: each would give every job its own time series.
func assertBounded(t *testing.T, r *telemetrytest.Reader, name string) {
	t.Helper()
	for _, p := range r.Points(t, name) {
		for k, v := range p.Attrs {
			if strings.Contains(v, "acme") || strings.Contains(v, "widgets") || strings.Contains(v, "nope") {
				t.Errorf("%s has attribute %s=%q, which names the repository", name, k, v)
			}
		}
	}
}

func TestJobsFinishedCompleted(t *testing.T) {
	t.Parallel()

	env, r := newMeteredEnv(t)
	report.Register(env, &report.Activities{})
	mockSmallRepository(env, 0)

	env.ExecuteWorkflow(report.RepositoryReport, request)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	points := r.Points(t, report.MetricJobsFinished)
	if len(points) != 1 {
		t.Fatalf("%s points = %+v, want one", report.MetricJobsFinished, points)
	}
	want := map[string]string{report.AttrJobOutcome: report.OutcomeCompleted, report.AttrErrorType: ""}
	if got := telemetrytest.Find(points, want); got != 1 {
		t.Errorf("%s%v = %v, want 1; points %+v", report.MetricJobsFinished, want, got, points)
	}
	assertBounded(t, r, report.MetricJobsFinished)
}

func TestJobsFinishedFailed(t *testing.T) {
	t.Parallel()

	env, r := newMeteredEnv(t)
	report.Register(env, newActivities(t, respond(http.StatusNotFound, `{"message":"Not Found"}`)))

	env.ExecuteWorkflow(report.RepositoryReport, report.Request{Owner: "acme", Repo: "nope"})
	assertFailed(t, env, report.ErrorTypeNotFound)

	finished := r.Points(t, report.MetricJobsFinished)
	want := map[string]string{report.AttrJobOutcome: report.OutcomeFailed, report.AttrErrorType: report.ErrorTypeNotFound}
	if got := telemetrytest.Find(finished, want); got != 1 {
		t.Errorf("%s%v = %v, want 1; points %+v", report.MetricJobsFinished, want, got, finished)
	}
	attempts := r.Points(t, report.MetricActivityAttempts)
	want = map[string]string{
		report.AttrActivityName:  report.ActivityFetchRepository,
		report.AttrActivityRetry: "false",
		report.AttrErrorType:     report.ErrorTypeNotFound,
	}
	if got := telemetrytest.Find(attempts, want); got != 1 {
		t.Errorf("%s%v = %v, want 1; points %+v", report.MetricActivityAttempts, want, got, attempts)
	}
	assertBounded(t, r, report.MetricJobsFinished)
	assertBounded(t, r, report.MetricActivityAttempts)
}

func TestJobsFinishedCanceled(t *testing.T) {
	t.Parallel()

	env, r := newMeteredEnv(t)
	report.Register(env, &report.Activities{})
	mockSmallRepository(env, time.Hour)
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Minute)

	env.ExecuteWorkflow(report.RepositoryReport, request)

	finished := r.Points(t, report.MetricJobsFinished)
	want := map[string]string{report.AttrJobOutcome: report.OutcomeCanceled, report.AttrErrorType: ""}
	if got := telemetrytest.Find(finished, want); got != 1 {
		t.Errorf("%s%v = %v, want 1; points %+v", report.MetricJobsFinished, want, got, finished)
	}
}

// A rate-limited page counts two attempts, the retry flagged, and records
// the wait before the retry, by kind, in the rate-limit buckets.
func TestRateLimitMetrics(t *testing.T) {
	t.Parallel()

	reset := strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10)
	issues := &sequence{handlers: []http.HandlerFunc{
		respond(http.StatusForbidden, primaryLimitBody,
			"X-RateLimit-Limit", "5000", "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset),
		issuesOK,
	}}
	env, r := newMeteredEnv(t)
	report.Register(env, newActivities(t, fakeRepository(issues)))

	env.ExecuteWorkflow(report.RepositoryReport, request)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}

	attempts := r.Points(t, report.MetricActivityAttempts)
	for _, tt := range []struct {
		attrs map[string]string
		want  float64
	}{
		{map[string]string{
			report.AttrActivityName: report.ActivityFetchIssuesPage, report.AttrActivityRetry: "false",
			report.AttrErrorType: report.ErrorTypeRateLimited,
		}, 1},
		{map[string]string{
			report.AttrActivityName: report.ActivityFetchIssuesPage, report.AttrActivityRetry: "true", report.AttrErrorType: "",
		}, 1},
		{map[string]string{report.AttrActivityName: report.ActivityFetchCommitsPage, report.AttrErrorType: ""}, 1},
	} {
		if got := telemetrytest.Find(attempts, tt.attrs); got != tt.want {
			t.Errorf("%s%v = %v, want %v; points %+v", report.MetricActivityAttempts, tt.attrs, got, tt.want, attempts)
		}
	}

	waits := r.Points(t, report.MetricRateLimitWait)
	if len(waits) != 1 || waits[0].Attrs[report.AttrRateLimitKind] != "primary" || waits[0].Value != 1 {
		t.Fatalf("%s points = %+v, want one primary wait", report.MetricRateLimitWait, waits)
	}
	if sum := waits[0].Sum; sum < 29*60 || sum > 31*60 {
		t.Errorf("rate-limit wait = %vs, want about 30 minutes", sum)
	}
	if got, want := waits[0].Bounds, report.RateLimitWaitBuckets(); !slices.Equal(got, want) {
		t.Errorf("rate-limit wait buckets = %v, want %v", got, want)
	}
	assertBounded(t, r, report.MetricActivityAttempts)
	assertBounded(t, r, report.MetricRateLimitWait)
}
