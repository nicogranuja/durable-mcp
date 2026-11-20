package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestPushEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		want    bool
		wantErr bool
	}{
		{"nothing set: off, no network calls", nil, false, false},
		{"exporter otlp", map[string]string{envMetricsExporter: "otlp"}, true, false},
		{"exporter console", map[string]string{envMetricsExporter: "console"}, true, false},
		{"exporter none", map[string]string{envMetricsExporter: "none"}, false, false},
		{"none wins over an endpoint", map[string]string{envMetricsExporter: "none", envOTLPEndpoint: "http://collector:4318"}, false, false},
		{"endpoint", map[string]string{envOTLPEndpoint: "http://collector:4318"}, true, false},
		{"metrics endpoint", map[string]string{envOTLPMetricsEndpoint: "http://collector:4318/v1/metrics"}, true, false},
		{"prometheus is -metrics-addr's job", map[string]string{envMetricsExporter: "otlp, prometheus"}, false, true},
	}
	for _, tt := range tests {
		got, err := pushEnabled(env(tt.env))
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("%s: pushEnabled() = %v, %v, want %v, error %v", tt.name, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestNewIsOffByDefault(t *testing.T) {
	t.Parallel()

	tel, err := New(t.Context(), Config{Component: "serve", Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tel.MeterProvider().(noop.MeterProvider); !ok {
		t.Errorf("meter provider = %T, want the no-op one", tel.MeterProvider())
	}
	if tel.Pushing() || tel.PrometheusHandler() != nil {
		t.Errorf("Pushing() = %v, PrometheusHandler() = %v, want neither", tel.Pushing(), tel.PrometheusHandler())
	}
	if err := tel.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown() = %v", err)
	}
}

// The Prometheus endpoint serves the durable-mcp and Temporal SDK metrics,
// with the views' buckets, from a private registry.
func TestPrometheusHandler(t *testing.T) {
	t.Parallel()

	logs := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	tel, err := New(t.Context(), Config{Component: "worker", Version: "v1.2.3", Prometheus: true, Getenv: env(nil), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	h := tel.TemporalMetricsHandler()
	h.WithTags(map[string]string{report.AttrJobOutcome: report.OutcomeCompleted}).Counter(report.MetricJobsFinished).Inc(1)
	h.Timer("temporal_activity_execution_latency").Record(30 * time.Millisecond)
	h.WithTags(map[string]string{report.AttrRateLimitKind: "primary"}).Timer(report.MetricRateLimitWait).Record(20 * time.Minute)
	// The handler's instrument errors are logged, not raised as a panic.
	h.Counter("not a valid name!").Inc(1)

	rec := httptest.NewRecorder()
	tel.PrometheusHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	out := string(body)
	for _, want := range []string{
		`durable_mcp_jobs_finished_total{durable_mcp_job_outcome="completed"`,
		// A monotonic counter, not an up-down one (a gauge).
		"# TYPE durable_mcp_jobs_finished_total counter",
		`temporal_activity_execution_latency_seconds_bucket{otel_scope_name="go.temporal.io/sdk"`,
		`le="0.001"`,
		`durable_mcp_github_rate_limit_wait_duration_seconds_bucket{durable_mcp_github_rate_limit_kind="primary"`,
		`le="960"`,
		`target_info{durable_mcp_component="worker"`,
		`service_name="durable-mcp"`,
		`service_version="v1.2.3"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics does not contain %s:\n%s", want, out)
		}
	}
	if !strings.Contains(logs.String(), "Temporal SDK metric failed") {
		t.Errorf("logs = %q, want the invalid instrument's error", logs.String())
	}
	if strings.Contains(out, "go_goroutines") || strings.Contains(out, "process_") {
		t.Errorf("/metrics has the default registry's collectors:\n%s", out)
	}
}
