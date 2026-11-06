package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nicogranuja/durable-mcp/internal/mcpserver"
)

func TestParseServeFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want serveConfig
	}{
		{
			name: "defaults suit a local development server",
			want: serveConfig{
				temporal:  temporalConfig{address: "localhost:7233", namespace: "default", taskQueue: "durable-mcp"},
				transport: "stdio",
				httpAddr:  "127.0.0.1:8080",
			},
		},
		{
			name: "environment",
			env: map[string]string{
				"TEMPORAL_ADDRESS": "temporal.internal:7233", "DURABLE_MCP_TASK_QUEUE": "reports",
				"DURABLE_MCP_TRANSPORT": "http", "DURABLE_MCP_HTTP_ADDR": "[::1]:9000",
			},
			want: serveConfig{
				temporal:  temporalConfig{address: "temporal.internal:7233", namespace: "default", taskQueue: "reports"},
				transport: "http",
				httpAddr:  "[::1]:9000",
			},
		},
		{
			name: "flags override the environment",
			args: []string{"-transport", "http", "-http-addr", "127.0.0.1:0", "-task-queue", "q"},
			env:  map[string]string{"DURABLE_MCP_TRANSPORT": "stdio", "DURABLE_MCP_TASK_QUEUE": "reports"},
			want: serveConfig{
				temporal:  temporalConfig{address: "localhost:7233", namespace: "default", taskQueue: "q"},
				transport: "http",
				httpAddr:  "127.0.0.1:0",
			},
		},
		{
			// The address only matters for the http transport.
			name: "stdio ignores the HTTP address",
			args: []string{"-http-addr", "0.0.0.0:8080"},
			want: serveConfig{
				temporal:  temporalConfig{address: "localhost:7233", namespace: "default", taskQueue: "durable-mcp"},
				transport: "stdio",
				httpAddr:  "0.0.0.0:8080",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			got, err := parseServeFlags(tt.args, env(tt.env), &stderr)
			if err != nil {
				t.Fatalf("parseServeFlags() error = %v (stderr: %s)", err, stderr.String())
			}
			if got != tt.want {
				t.Errorf("parseServeFlags() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCheckLoopback(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"127.0.0.1:8080", "127.0.0.2:1", "[::1]:8080", "127.0.0.1:0", "[::ffff:127.0.0.1]:8080"} {
		if err := checkLoopback(addr); err != nil {
			t.Errorf("checkLoopback(%q) = %v, want nil", addr, err)
		}
	}
	for _, addr := range []string{
		":8080", "0.0.0.0:8080", "[::]:8080", "192.168.1.10:8080", "example.com:80", "127.0.0.1", "localhost:8080",
	} {
		if err := checkLoopback(addr); err == nil {
			t.Errorf("checkLoopback(%q) = nil, want an error", addr)
		}
	}
}

// closedAddr returns an address that refuses connections.
func closedAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// syncBuffer is a bytes.Buffer that is safe for concurrent use, for
// stderr written by a running server.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// TestServeStdio runs the serve command over pipes, as an MCP host such as
// Claude Code does, with Temporal down: the server still starts and lists
// its tools, and exits when its stdin closes.
func TestServeStdio(t *testing.T) {
	t.Parallel()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	var stderr syncBuffer
	addr := closedAddr(t)
	done := make(chan int, 1)
	go func() {
		done <- run(t.Context(), []string{"serve", "-temporal-address", addr}, env(nil), stdinR, stdoutW, &stderr)
		stdoutW.Close()
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-host"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: stdoutR, Writer: stdinW}, nil)
	if err != nil {
		t.Fatalf("Connect: %v (stderr: %s)", err, stderr.String())
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 4 {
		t.Fatalf("ListTools() = %v, %v; want 4 tools", tools, err)
	}
	// A call that needs no Temporal round trip.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "job_status", Arguments: map[string]any{"job_id": "nope"}})
	if err != nil || !res.IsError {
		t.Errorf("CallTool(job_status) = %+v, %v; want a tool error", res, err)
	}
	cs.Close() // closes the server's stdin

	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
		}
	case <-ctx.Done():
		t.Fatal("serve did not exit after stdin closed")
	}
	if got := stderr.String(); !strings.Contains(got, "Temporal is not reachable yet") {
		t.Errorf("stderr = %q, want a warning that Temporal is down", got)
	}
}

// noTemporal is a Temporal client for tests that must not reach Temporal:
// any call panics.
type noTemporal struct{ mcpserver.Client }

func TestHTTPHandler(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(httpHandler(mcpserver.New(noTemporal{}, mcpserver.Options{}), discardLogger()))
	t.Cleanup(srv.Close)

	t.Run("streamable HTTP client", func(t *testing.T) {
		t.Parallel()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-host"}, nil).
			Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: srv.URL + mcpPath}, nil)
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer cs.Close()
		tools, err := cs.ListTools(t.Context(), nil)
		if err != nil || len(tools.Tools) != 4 {
			t.Fatalf("ListTools() = %v, %v; want 4 tools", tools, err)
		}
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "job_result", Arguments: map[string]any{"job_id": "x"}})
		if err != nil || !res.IsError {
			t.Errorf("CallTool(job_result) = %+v, %v; want a tool error", res, err)
		}
	})

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
	tests := []struct {
		name   string
		path   string
		header map[string]string
		want   int
	}{
		{name: "same origin", path: mcpPath, want: http.StatusOK},
		{name: "other paths", path: "/", want: http.StatusNotFound},
		{
			// A page in the user's browser posting to the local server.
			name:   "cross-origin browser request",
			path:   mcpPath,
			header: map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"},
			want:   http.StatusForbidden,
		},
		{
			// DNS rebinding: a name the attacker controls, resolving to
			// 127.0.0.1.
			name:   "non-loopback Host",
			path:   mcpPath,
			header: map[string]string{"Host": "evil.example"},
			want:   http.StatusForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+tt.path, strings.NewReader(initialize))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			for k, v := range tt.header {
				if k == "Host" {
					req.Host = v
				}
				req.Header.Set(k, v)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestServeHTTPStops(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- serveHTTP(ctx, mcpserver.New(noTemporal{}, mcpserver.Options{}), "127.0.0.1:0", discardLogger())
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveHTTP() = %v, want nil after the context is canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serveHTTP did not stop")
	}
}
