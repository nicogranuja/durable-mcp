package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/nicogranuja/durable-mcp/internal/telemetry"
)

const (
	// metricsPath is where -metrics-addr serves the Prometheus metrics.
	metricsPath = "/metrics"

	envMetricsAddr = "DURABLE_MCP_METRICS_ADDR"

	// telemetryShutdownTimeout bounds the export of the last metrics when
	// a command stops.
	telemetryShutdownTimeout = 5 * time.Second
)

// metricsConfig says where to serve Prometheus metrics. OTLP export is
// configured through the standard OTEL_* environment variables instead;
// see the telemetry package.
type metricsConfig struct {
	// addr is the host:port to serve metricsPath on, or empty for none.
	addr string
}

// register defines the -metrics-addr flag on fs.
func (c *metricsConfig) register(fs *flag.FlagSet, getenv func(string) string) {
	fs.StringVar(&c.addr, "metrics-addr", getenv(envMetricsAddr),
		"`host:port` to serve Prometheus metrics on, at "+metricsPath+"; off if empty. Without a host, as in :9464, "+
			"it listens on 127.0.0.1; name the host, such as 0.0.0.0:9464, to listen beyond loopback (env "+envMetricsAddr+")")
}

// check validates the address and gives a bare :port the loopback host,
// so that metrics stay on this machine unless the operator names a host.
func (c *metricsConfig) check() error {
	if c.addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(c.addr)
	if err != nil {
		return fmt.Errorf("-metrics-addr %q is not host:port: %w", c.addr, err)
	}
	if host == "" {
		c.addr = net.JoinHostPort("127.0.0.1", port)
	}
	return nil
}

// startTelemetry sets up metrics for the component ("serve" or "worker")
// and, with an address, serves them for Prometheus until stop is called.
// stop shuts the metrics server down and exports the last metrics; call it
// once the component is done recording, after the Temporal client closes.
func startTelemetry(ctx context.Context, component string, cfg metricsConfig, logger *slog.Logger) (
	tel *telemetry.Telemetry, stop func(), err error,
) {
	tel, err = telemetry.New(ctx, telemetry.Config{
		Component:  component,
		Version:    version,
		Prometheus: cfg.addr != "",
		Logger:     logger,
	})
	if err != nil {
		return nil, nil, err
	}
	shutdown := func() {
		// The command's context is already canceled when it stops.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryShutdownTimeout)
		defer cancel()
		if err := tel.Shutdown(shutdownCtx); err != nil {
			logger.Warn("Could not export the last metrics", "Error", err)
		}
	}
	if tel.Pushing() {
		logger.Info("Exporting metrics over OTLP, as the OTEL_* environment variables configure")
	}
	if cfg.addr == "" {
		return tel, shutdown, nil
	}

	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.addr)
	if err != nil {
		shutdown()
		return nil, nil, fmt.Errorf("listen for metrics on %s: %w", cfg.addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+metricsPath, tel.PrometheusHandler())
	srv := newHTTPServer(mux, logger)
	serveCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := serveUntilDone(serveCtx, srv, ln, httpShutdownTimeout); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("Metrics server failed", "Error", err)
		}
	}()
	logger.Info("Serving Prometheus metrics", "URL", "http://"+ln.Addr().String()+metricsPath)
	return tel, func() {
		cancel()
		<-done
		shutdown()
	}, nil
}
