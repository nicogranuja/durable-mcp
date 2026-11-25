package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go.temporal.io/sdk/client"
)

// Paths of the HTTP transport's health endpoints.
const (
	healthzPath = "/healthz"
	readyzPath  = "/readyz"
)

// readinessTimeout bounds the Temporal check behind /readyz, so a probe
// gets an answer well within the usual one-second-or-more probe timeouts
// of load balancers and Kubernetes even when Temporal does not answer.
const readinessTimeout = 2 * time.Second

// readinessCheck reports whether serve can do its work: nil if it can.
type readinessCheck func(context.Context) error

// temporalReady returns a readiness check that asks Temporal's health
// service, the same check serve makes at startup.
func temporalReady(c client.Client) readinessCheck {
	return func(ctx context.Context) error {
		_, err := c.CheckHealth(ctx, &client.CheckHealthRequest{})
		return err
	}
}

// addHealthHandlers adds the health endpoints to mux:
//
//   - /healthz, liveness: 200 whenever the process can answer HTTP. It
//     checks nothing else, so that a Temporal outage never gets a healthy
//     serve restarted.
//   - /readyz, readiness: 200 if Temporal answers a health check within
//     readinessTimeout, 503 if not. A serve whose tools would only say
//     that Temporal is unreachable can be taken out of a load balancer.
//
// Both take GET and HEAD only, and need no token, so probes can reach
// them; that is also why they answer with nothing but "ok" or
// "unavailable": no version, address, or error. They are outside auth and
// telemetry, so probes neither count as rejected requests nor add spans.
func addHealthHandlers(mux *http.ServeMux, ready readinessCheck, logger *slog.Logger) {
	mux.HandleFunc("GET "+healthzPath, func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET "+readyzPath, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		if err := ready(ctx); err != nil {
			// Debug, not Warn: a probe every few seconds during an outage
			// would flood the log, and startup already warns.
			logger.Debug("Readiness check failed: Temporal is not reachable", "Error", err)
			writeHealth(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		writeHealth(w, http.StatusOK, "ok")
	})
}

// writeHealth writes a health endpoint's answer.
func writeHealth(w http.ResponseWriter, code int, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body+"\n")
}
