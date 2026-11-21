package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestTracesEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"nothing set: off, no network calls", nil, false},
		{"exporter otlp", map[string]string{envTracesExporter: "otlp"}, true},
		{"exporter console", map[string]string{envTracesExporter: " console "}, true},
		{"exporter none", map[string]string{envTracesExporter: "none"}, false},
		{"none wins over an endpoint", map[string]string{envTracesExporter: "none", envOTLPEndpoint: "http://collector:4318"}, false},
		{"endpoint", map[string]string{envOTLPEndpoint: "http://collector:4318"}, true},
		{"traces endpoint", map[string]string{envOTLPTracesEndpoint: "http://collector:4318/v1/traces"}, true},
		{"a metrics exporter alone", map[string]string{envMetricsExporter: "otlp"}, false},
	}
	for _, tt := range tests {
		if got := tracesEnabled(env(tt.env)); got != tt.want {
			t.Errorf("%s: tracesEnabled() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestTracesOffByDefault(t *testing.T) {
	t.Parallel()

	tel, err := New(t.Context(), Config{Component: "worker", Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tel.TracerProvider().(tracenoop.TracerProvider); !ok || tel.Tracing() {
		t.Errorf("tracer provider = %T, Tracing() = %v, want the no-op one", tel.TracerProvider(), tel.Tracing())
	}
	if is, err := tel.TemporalInterceptors(); len(is) != 0 || err != nil {
		t.Errorf("TemporalInterceptors() = %v, %v, want none", is, err)
	}
}

// keepOnShutdown is an in-memory exporter that keeps its spans when it
// shuts down, so that a test can see what the shutdown flushed.
type keepOnShutdown struct{ *tracetest.InMemoryExporter }

func (keepOnShutdown) Shutdown(context.Context) error { return nil }

// With the batching the commands use, Shutdown exports the spans not yet
// exported.
func TestShutdownFlushesSpans(t *testing.T) {
	t.Parallel()

	exp := keepOnShutdown{tracetest.NewInMemoryExporter()}
	tp := NewTracerProvider(nil, exp, true)
	tel := &Telemetry{tracerProvider: tp, tracing: true, shutdown: []func(context.Context) error{tp.Shutdown}}
	_, span := tel.TracerProvider().Tracer("test").Start(t.Context(), "op")
	span.End()
	if got := len(exp.GetSpans()); got != 0 {
		t.Fatalf("exported %d spans before Shutdown, want them batched", got)
	}
	if err := tel.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(exp.GetSpans()); got != 1 {
		t.Errorf("exported %d spans, want 1", got)
	}
}

func TestSpanPolicy(t *testing.T) {
	t.Parallel()

	const canary = "ghp_CANARYcanaryCANARYcanary0123456789"
	exp := tracetest.NewInMemoryExporter()
	tp := NewTracerProvider(nil, exp, false)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, span := tp.Tracer("test").Start(t.Context(), "GET Bearer "+canary,
		trace.WithAttributes(
			attribute.String("url.full", "https://user:"+canary+"@api.github.com/repos/o/r/issues?page=2&per_page=100&access_token="+canary+"#"+canary),
			attribute.String("url.query", "state=open&sig="+canary),
			attribute.String("http.request.header.authorization", "Bearer "+canary),
			attribute.StringSlice("http.request.header.x-api-key", []string{canary}),
			attribute.StringSlice("notes", []string{"token " + canary}),
			attribute.Int("http.response.status_code", 401),
		))
	span.AddEvent("exception", trace.WithAttributes(attribute.String("exception.message",
		`Get "https://api.github.com/x?client_secret=`+canary+`": context deadline exceeded`)))
	span.SetStatus(codes.Error, "Authorization: basic "+canary)
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	s := spans[0]
	attrs := map[string]attribute.Value{}
	for _, kv := range s.Attributes {
		attrs[string(kv.Key)] = kv.Value
	}
	if got, want := attrs["url.full"].AsString(), "https://api.github.com/repos/o/r/issues?access_token=REDACTED&page=2&per_page=100"; got != want {
		t.Errorf("url.full = %q, want %q", got, want)
	}
	if got, want := attrs["url.query"].AsString(), "sig=REDACTED&state=open"; got != want {
		t.Errorf("url.query = %q, want %q", got, want)
	}
	for _, k := range []string{"http.request.header.authorization", "http.request.header.x-api-key"} {
		if _, ok := attrs[k]; ok {
			t.Errorf("attribute %s was kept, want it dropped", k)
		}
	}
	if got := attrs["http.response.status_code"].AsInt64(); got != 401 {
		t.Errorf("status code = %d, want it kept", got)
	}
	assertNoCanary(t, s, canary)
}

// TestHTTPInstrumentation checks the server and client span names, and
// that the client sends no trace context to the third party it calls.
func TestHTTPInstrumentation(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	tel, err := New(t.Context(), Config{Component: "serve", Getenv: env(nil), SpanExporter: exp})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	var gotTraceparent string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("Traceparent")
	}))
	t.Cleanup(backend.Close)
	hc := &http.Client{Transport: tel.HTTPTransport(nil)}
	front := httptest.NewServer(tel.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, backend.URL, nil)
		if resp, err := hc.Do(req); err == nil {
			resp.Body.Close()
		}
	}), "/mcp"))
	t.Cleanup(front.Close)

	req, _ := http.NewRequestWithContext(t.Context(), "BREW", front.URL+"/mcp", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var names []string
	for _, s := range exp.GetSpans() {
		names = append(names, s.Name)
	}
	if got, want := strings.Join(names, ","), "GET,HTTP /mcp"; got != want {
		t.Errorf("span names = %s, want %s", got, want)
	}
	if gotTraceparent != "" {
		t.Errorf("the GitHub transport sent traceparent %q, want none", gotTraceparent)
	}
}

// assertNoCanary fails if canary appears anywhere in s.
func assertNoCanary(t *testing.T, s tracetest.SpanStub, canary string) {
	t.Helper()
	check := func(where, v string) {
		if strings.Contains(v, canary) {
			t.Errorf("span %q: %s holds the canary: %q", s.Name, where, v)
		}
	}
	check("name", s.Name)
	check("status", s.Status.Description)
	for _, kv := range s.Attributes {
		check(string(kv.Key), kv.Value.Emit())
	}
	for _, e := range s.Events {
		check("event", e.Name)
		for _, kv := range e.Attributes {
			check("event "+string(kv.Key), kv.Value.Emit())
		}
	}
}
