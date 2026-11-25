package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/log"

	"github.com/nicogranuja/durable-mcp/internal/auth"
	"github.com/nicogranuja/durable-mcp/internal/mcpserver"
)

// alwaysReady is a readiness check for tests that do not test readiness.
func alwaysReady(context.Context) error { return nil }

// get sends a request to srv and returns the status and body.
func get(t *testing.T, srv *httptest.Server, method, path string, header map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		if k == "Host" {
			req.Host = v
		}
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body), resp.Header
}

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()

	tokens, err := auth.ParseTokenList(auth.Entry("alice", aliceToken))
	if err != nil {
		t.Fatal(err)
	}
	// The error a failing check returns, which must not reach the answer.
	secret := errors.New("dial tcp temporal.internal:7233: connection refused")
	var deadline atomic.Int64 // the check's time left, set by the server's goroutine
	ready := func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if !ok {
			return errors.New("no deadline")
		}
		deadline.Store(int64(time.Until(d)))
		return nil
	}
	notReady := func(context.Context) error { return secret }

	server := func(check readinessCheck, tokens *auth.Tokens) *httptest.Server {
		srv := httptest.NewServer(httpHandler(mcpserver.New(noTemporal{}, mcpserver.Options{}), check, tokens, nil, discardLogger()))
		t.Cleanup(srv.Close)
		return srv
	}
	withTokens := server(ready, tokens)
	withTokensDown := server(notReady, tokens)
	noTokensDown := server(notReady, nil)

	tests := []struct {
		name     string
		srv      *httptest.Server
		method   string
		path     string
		header   map[string]string
		want     int
		wantBody string
	}{
		{name: "liveness without a token", srv: withTokens, method: http.MethodGet, path: healthzPath, want: http.StatusOK, wantBody: "ok\n"},
		{name: "liveness while Temporal is down", srv: withTokensDown, method: http.MethodGet, path: healthzPath, want: http.StatusOK, wantBody: "ok\n"},
		{name: "liveness, HEAD", srv: withTokens, method: http.MethodHead, path: healthzPath, want: http.StatusOK},
		{name: "readiness without a token", srv: withTokens, method: http.MethodGet, path: readyzPath, want: http.StatusOK, wantBody: "ok\n"},
		{name: "readiness while Temporal is down", srv: withTokensDown, method: http.MethodGet, path: readyzPath, want: http.StatusServiceUnavailable, wantBody: "unavailable\n"},
		{name: "readiness without tokens", srv: noTokensDown, method: http.MethodGet, path: readyzPath, want: http.StatusServiceUnavailable, wantBody: "unavailable\n"},
		{
			// Probes from a kubelet or load balancer send their own Host.
			name: "liveness with a non-loopback Host", srv: noTokensDown, method: http.MethodGet, path: healthzPath,
			header: map[string]string{"Host": "10.0.0.7:8080"}, want: http.StatusOK, wantBody: "ok\n",
		},
		{name: "liveness, POST", srv: withTokens, method: http.MethodPost, path: healthzPath, want: http.StatusMethodNotAllowed},
		{name: "readiness, DELETE", srv: withTokens, method: http.MethodDelete, path: readyzPath, want: http.StatusMethodNotAllowed},
		{name: "a token presented anyway is not checked", srv: withTokens, method: http.MethodGet, path: healthzPath,
			header: map[string]string{"Authorization": "Bearer wrong"}, want: http.StatusOK, wantBody: "ok\n"},
		// The MCP endpoint itself still needs a token.
		{name: "MCP without a token", srv: withTokens, method: http.MethodPost, path: mcpPath, want: http.StatusUnauthorized},
		{name: "a path below liveness", srv: withTokens, method: http.MethodGet, path: healthzPath + "/x", want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body, header := get(t, tt.srv, tt.method, tt.path, tt.header)
			if code != tt.want {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.path, code, tt.want)
			}
			if tt.wantBody != "" && body != tt.wantBody {
				t.Errorf("%s %s body = %q, want %q", tt.method, tt.path, body, tt.wantBody)
			}
			if strings.Contains(body, "temporal.internal") || strings.Contains(body, "refused") {
				t.Errorf("%s %s body = %q, which reveals the check's error", tt.method, tt.path, body)
			}
			if tt.wantBody != "" && header.Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", header.Get("Cache-Control"))
			}
		})
	}
	if d := time.Duration(deadline.Load()); d <= 0 || d > readinessTimeout {
		t.Errorf("readiness check deadline = %v from the request, want at most %v", d, readinessTimeout)
	}
}

// TestTemporalReady checks the readiness check against a Temporal client
// that cannot connect: it fails, within the deadline.
func TestTemporalReady(t *testing.T) {
	t.Parallel()

	c, err := client.NewLazyClient(client.Options{HostPort: closedAddr(t), Logger: log.NewStructuredLogger(discardLogger())})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(t.Context(), readinessTimeout)
	defer cancel()
	start := time.Now()
	if err := temporalReady(c)(ctx); err == nil {
		t.Error("temporalReady() = nil with no Temporal server, want an error")
	}
	if elapsed := time.Since(start); elapsed > readinessTimeout+time.Second {
		t.Errorf("temporalReady() took %v, want at most about %v", elapsed, readinessTimeout)
	}
}
