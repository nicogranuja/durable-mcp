// Package telemetry sets up OpenTelemetry metrics for the serve and worker
// commands.
//
// Telemetry is off unless it is asked for, so that a default run makes no
// network calls and opens no ports. There are two ways to turn it on, and
// both can be used at once:
//
//   - Push, over OTLP: set OTEL_METRICS_EXPORTER (otlp, or console for
//     stdout), or an OTLP endpoint (OTEL_EXPORTER_OTLP_ENDPOINT or
//     OTEL_EXPORTER_OTLP_METRICS_ENDPOINT). The standard OTEL_* variables
//     configure the exporter, through autoexport.
//   - Pull, by Prometheus: pass a -metrics-addr, and the command serves
//     /metrics there from a private registry, so that only these metrics,
//     and not the Go client library's process collectors, are exposed.
//
// The Temporal SDK's own metrics (temporal_*) go through the same meter
// provider, through [Telemetry.TemporalMetricsHandler].
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.temporal.io/sdk/client"
	temporalotel "go.temporal.io/sdk/contrib/opentelemetry"
)

// ServiceName is the service.name resource attribute, unless
// OTEL_SERVICE_NAME overrides it.
const ServiceName = "durable-mcp"

// Environment variables that turn on the OTLP (push) exporter.
const (
	envMetricsExporter      = "OTEL_METRICS_EXPORTER"
	envOTLPEndpoint         = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envOTLPMetricsEndpoint  = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	exporterNone            = "none"
	exporterPrometheus      = "prometheus"
	temporalMeterScope      = "go.temporal.io/sdk"
	componentAttributeKey   = "durable_mcp.component"
	temporalHistogramFilter = "temporal_*"
)

// Config configures [New].
type Config struct {
	// Component names the command, "serve" or "worker". It becomes the
	// durable_mcp.component resource attribute, as both commands share a
	// service name.
	Component string

	// Version is the service.version resource attribute.
	Version string

	// Prometheus serves the metrics for a Prometheus scrape from
	// [Telemetry.PrometheusHandler].
	Prometheus bool

	// Getenv reads the environment, for the decision whether to push
	// metrics. Nil means os.Getenv. The exporter itself reads the OTEL_*
	// variables from the process environment.
	Getenv func(string) string

	// Logger receives errors from the Temporal SDK's metrics. Nil
	// discards them.
	Logger *slog.Logger
}

// Telemetry holds the configured meter provider.
type Telemetry struct {
	meterProvider metric.MeterProvider
	shutdown      func(context.Context) error
	prometheus    http.Handler
	logger        *slog.Logger
	pushing       bool
}

// New sets up metrics as cfg and the environment say. With neither, it
// returns a Telemetry whose meter provider records nothing.
//
// Call Shutdown when the command stops, so that the last metrics are
// exported.
func New(ctx context.Context, cfg Config) (*Telemetry, error) {
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	t := &Telemetry{
		meterProvider: noop.NewMeterProvider(),
		shutdown:      func(context.Context) error { return nil },
		logger:        logger,
	}

	push, err := pushEnabled(getenv)
	if err != nil {
		return nil, err
	}
	var readers []sdkmetric.Reader
	if push {
		r, err := autoexport.NewMetricReader(ctx)
		if err != nil {
			return nil, fmt.Errorf("configure the OTLP metrics exporter: %w", err)
		}
		if !autoexport.IsNoneMetricReader(r) {
			readers = append(readers, r)
			t.pushing = true
		}
	}
	if cfg.Prometheus {
		// A private registry: the default one would also expose the
		// client library's Go and process collectors, and anything else
		// that registers there.
		reg := prometheus.NewRegistry()
		exp, err := promexporter.New(promexporter.WithRegisterer(reg))
		if err != nil {
			return nil, fmt.Errorf("configure the Prometheus exporter: %w", err)
		}
		readers = append(readers, exp)
		t.prometheus = promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
			// A scrape still gets the metrics that did gather.
			ErrorHandling: promhttp.ContinueOnError,
		})
	}
	if len(readers) == 0 {
		return t, nil
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(cfg.Version),
		componentAttribute(cfg.Component),
	))
	if err != nil {
		return nil, fmt.Errorf("build the telemetry resource: %w", err)
	}
	// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES win over the defaults.
	if env, err := resource.New(ctx, resource.WithFromEnv()); err == nil {
		if merged, err := resource.Merge(res, env); err == nil {
			res = merged
		}
	}
	mp := NewMeterProvider(res, readers...)
	t.meterProvider = mp
	t.shutdown = mp.Shutdown
	return t, nil
}

// pushEnabled reports whether the environment asks for metrics to be
// pushed over OTLP. autoexport alone would push to localhost:4318 when
// OTEL_METRICS_EXPORTER is unset; here, unset means off.
func pushEnabled(getenv func(string) string) (bool, error) {
	exporters := strings.TrimSpace(getenv(envMetricsExporter))
	if exporters != "" {
		names := strings.Split(exporters, ",")
		for i := range names {
			names[i] = strings.TrimSpace(names[i])
		}
		if slices.Contains(names, exporterPrometheus) {
			// autoexport's Prometheus reader serves the global registry on
			// its own port; -metrics-addr serves a private one.
			return false, errors.New(envMetricsExporter + "=prometheus is not supported: use -metrics-addr to serve /metrics")
		}
		return !slices.Equal(names, []string{exporterNone}), nil
	}
	return getenv(envOTLPEndpoint) != "" || getenv(envOTLPMetricsEndpoint) != "", nil
}

// MeterProvider returns the meter provider to create instruments with.
func (t *Telemetry) MeterProvider() metric.MeterProvider { return t.meterProvider }

// Pushing reports whether metrics are pushed over OTLP.
func (t *Telemetry) Pushing() bool { return t.pushing }

// PrometheusHandler returns the handler that serves the metrics for a
// Prometheus scrape, or nil if [Config.Prometheus] was false.
func (t *Telemetry) PrometheusHandler() http.Handler { return t.prometheus }

// TemporalMetricsHandler returns the Temporal SDK metrics handler for the
// client options, backed by the meter provider. See [TemporalMetricsHandler].
func (t *Telemetry) TemporalMetricsHandler() client.MetricsHandler {
	return TemporalMetricsHandler(t.meterProvider, t.logger)
}

// Shutdown exports the metrics not yet exported and stops the exporters.
// It returns when ctx ends, whether or not they are done.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	return t.shutdown(ctx)
}

// TemporalMetricsHandler returns a Temporal SDK metrics handler that
// records to mp. Its counters are monotonic, so exporters see them as
// counters (Prometheus _total) rather than gauges. An instrument error is
// logged: the handler's default is to panic, which would take a worker down
// over a metric.
func TemporalMetricsHandler(mp metric.MeterProvider, logger *slog.Logger) client.MetricsHandler {
	return temporalotel.NewMetricsHandler(temporalotel.MetricsHandlerOptions{
		Meter:                mp.Meter(temporalMeterScope),
		UseMonotonicCounters: true,
		OnError: func(err error) {
			logger.Warn("Temporal SDK metric failed", "Error", err)
		},
	})
}
