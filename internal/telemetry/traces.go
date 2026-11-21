package telemetry

import (
	"net/http"
	"slices"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	temporalotel "go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/interceptor"
)

// Environment variables that turn on trace export.
const (
	envTracesExporter     = "OTEL_TRACES_EXPORTER"
	envOTLPTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	temporalTracerScope   = "go.temporal.io/sdk"
)

// tracesEnabled reports whether the environment asks for traces to be
// exported. As for metrics, autoexport alone would export to
// localhost:4318 when OTEL_TRACES_EXPORTER is unset; here, unset means off.
func tracesEnabled(getenv func(string) string) bool {
	if exporters := strings.TrimSpace(getenv(envTracesExporter)); exporters != "" {
		names := strings.Split(exporters, ",")
		for i := range names {
			names[i] = strings.TrimSpace(names[i])
		}
		return !slices.Equal(names, []string{exporterNone})
	}
	return getenv(envOTLPEndpoint) != "" || getenv(envOTLPTracesEndpoint) != ""
}

// NewTracerProvider returns a tracer provider that exports to exp through
// the span policy (see redactingExporter). With batch false, each span is
// exported as it ends, for tests.
//
// The sampler is the SDK's: parent-based, always on, unless
// OTEL_TRACES_SAMPLER says otherwise.
func NewTracerProvider(res *resource.Resource, exp sdktrace.SpanExporter, batch bool) *sdktrace.TracerProvider {
	exp = redactingExporter{next: exp}
	process := sdktrace.WithSyncer(exp)
	if batch {
		process = sdktrace.WithBatcher(exp)
	}
	return sdktrace.NewTracerProvider(sdktrace.WithResource(res), process)
}

// TracerProvider returns the tracer provider to create spans with. It
// records nothing unless traces are on.
func (t *Telemetry) TracerProvider() trace.TracerProvider { return t.tracerProvider }

// Tracing reports whether traces are exported.
func (t *Telemetry) Tracing() bool { return t.tracing }

// TemporalInterceptors returns the Temporal client interceptors: with
// traces on, the SDK's tracing interceptor, which carries the trace from a
// client call, such as starting a workflow, through the workflow and its
// activities, in the Temporal headers. Set on the client, it applies to the
// workers created from it too. With traces off, there are none.
func (t *Telemetry) TemporalInterceptors() ([]interceptor.ClientInterceptor, error) {
	if !t.tracing {
		return nil, nil
	}
	i, err := temporalotel.NewTracingInterceptor(temporalotel.TracerOptions{
		Tracer: t.tracerProvider.Tracer(temporalTracerScope),
	})
	if err != nil {
		return nil, err
	}
	return []interceptor.ClientInterceptor{i}, nil
}

// HTTPHandler instruments a server handler: a span per request, which
// continues a W3C traceparent the client sends (but takes no baggage from
// it), and the http.server.request.duration metric. The span is named
// after the method, bounded to the standard ones, and route.
func (t *Telemetry) HTTPHandler(h http.Handler, route string) http.Handler {
	return otelhttp.NewHandler(h, route,
		otelhttp.WithTracerProvider(t.tracerProvider),
		otelhttp.WithMeterProvider(t.meterProvider),
		otelhttp.WithPropagators(propagation.TraceContext{}),
		otelhttp.WithSpanNameFormatter(func(route string, r *http.Request) string {
			return boundedMethod(r.Method) + " " + route
		}),
	)
}

// HTTPTransport instruments a client transport (nil means
// http.DefaultTransport): a span per request and the
// http.client.request.duration metric. It sends no trace context: the
// client calls a third party, GitHub, which has no use for it.
func (t *Telemetry) HTTPTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base,
		otelhttp.WithTracerProvider(t.tracerProvider),
		otelhttp.WithMeterProvider(t.meterProvider),
		otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator()),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return boundedMethod(r.Method)
		}),
	)
}

// boundedMethod returns a standard HTTP method as is, and "HTTP" for any
// other, so that a client cannot create a span name per method it makes up.
func boundedMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return m
	}
	return "HTTP"
}
