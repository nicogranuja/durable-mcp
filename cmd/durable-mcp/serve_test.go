package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nicogranuja/durable-mcp/internal/auth"
	"github.com/nicogranuja/durable-mcp/internal/mcpserver"
)

// Test tokens are obviously fake: they are not in the minted format.
const (
	aliceToken = "test-token-alice-not-a-secret"
	bobToken   = "test-token-bob-not-a-secret"
)

// tokensFile writes token entries to a file and returns its path.
func tokensFile(t *testing.T, entries ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte(strings.Join(entries, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

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

func TestParseServeFlagsAuth(t *testing.T) {
	t.Parallel()

	file := tokensFile(t, "# callers", auth.Entry("alice", aliceToken), auth.Entry("bob", bobToken))
	emptyFile := tokensFile(t, "# no callers yet")
	list := auth.Entry("alice", aliceToken) + "," + auth.Entry("bob", bobToken)
	http := []string{"-transport", "http"}

	tests := []struct {
		name        string
		args        []string
		env         map[string]string
		wantCallers []string // nil: no tokens
		wantIgnored string
		wantErr     string
	}{
		{name: "no tokens", args: http},
		{name: "file flag", args: append(http, "-auth-tokens-file", file), wantCallers: []string{"alice", "bob"}},
		{name: "file from the environment", args: http, env: map[string]string{envAuthTokensFile: file}, wantCallers: []string{"alice", "bob"}},
		{name: "list from the environment", args: http, env: map[string]string{envAuthTokens: list}, wantCallers: []string{"alice", "bob"}},
		{
			name:    "file flag and list",
			args:    append(http, "-auth-tokens-file", file),
			env:     map[string]string{envAuthTokens: list},
			wantErr: "set either -auth-tokens-file (or DURABLE_MCP_AUTH_TOKENS_FILE) or DURABLE_MCP_AUTH_TOKENS, not both",
		},
		{
			name:    "file and list from the environment",
			args:    http,
			env:     map[string]string{envAuthTokensFile: file, envAuthTokens: list},
			wantErr: "not both",
		},
		{
			name:    "both on stdio too",
			env:     map[string]string{envAuthTokensFile: file, envAuthTokens: list},
			wantErr: "not both",
		},
		{name: "stdio ignores a file", env: map[string]string{envAuthTokensFile: file}, wantIgnored: "-auth-tokens-file"},
		{name: "stdio ignores a list", env: map[string]string{envAuthTokens: list}, wantIgnored: envAuthTokens},
		{
			// It is not even read.
			name:        "stdio ignores a missing file",
			args:        []string{"-auth-tokens-file", filepath.Join(t.TempDir(), "missing")},
			wantIgnored: "-auth-tokens-file",
		},
		{name: "missing file", args: append(http, "-auth-tokens-file", filepath.Join(t.TempDir(), "missing")), wantErr: "read the auth tokens"},
		{name: "empty file", args: append(http, "-auth-tokens-file", emptyFile), wantErr: "has no token entries"},
		{name: "bad entry", args: http, env: map[string]string{envAuthTokens: "alice:abc"}, wantErr: "DURABLE_MCP_AUTH_TOKENS: entry 1"},
		{
			// A token pasted where its hash belongs is not echoed.
			name:    "token instead of hash",
			args:    http,
			env:     map[string]string{envAuthTokens: "alice:" + auth.TokenPrefix + "notarealtokenatall"},
			wantErr: "holds a token, not its hash",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			got, err := parseServeFlags(tt.args, env(tt.env), &stderr)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseServeFlags() error = %v, want one containing %q", err, tt.wantErr)
				}
				if strings.Contains(stderr.String(), auth.TokenPrefix+"notarealtokenatall") {
					t.Errorf("stderr quotes the token: %s", stderr.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("parseServeFlags() error = %v (stderr: %s)", err, stderr.String())
			}
			var callers []string
			if got.tokens != nil {
				callers = got.tokens.Names()
			}
			if !slices.Equal(callers, tt.wantCallers) || (got.tokens == nil) != (tt.wantCallers == nil) {
				t.Errorf("callers = %q, want %q", callers, tt.wantCallers)
			}
			if got.ignoredTokens != tt.wantIgnored {
				t.Errorf("ignoredTokens = %q, want %q", got.ignoredTokens, tt.wantIgnored)
			}
		})
	}
}

// TestBindRule checks that the HTTP transport listens beyond loopback only
// with auth tokens, and warns that they travel over plain HTTP.
func TestBindRule(t *testing.T) {
	t.Parallel()

	tokenEnv := map[string]string{envAuthTokens: auth.Entry("alice", aliceToken)}
	tests := []struct {
		name     string
		addr     string
		env      map[string]string
		wantErr  bool
		wantWarn bool
	}{
		{name: "loopback without tokens", addr: "127.0.0.1:8080"},
		{name: "all interfaces without tokens", addr: "0.0.0.0:8080", wantErr: true},
		{name: "private address without tokens", addr: "192.168.1.10:8080", wantErr: true},
		{name: "loopback with tokens", addr: "[::1]:8080", env: tokenEnv},
		{name: "all interfaces with tokens", addr: "0.0.0.0:8080", env: tokenEnv, wantWarn: true},
		{name: "any host with tokens", addr: ":8080", env: tokenEnv, wantWarn: true},
		{name: "not host:port, even with tokens", addr: "0.0.0.0", env: tokenEnv, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := parseServeFlags([]string{"-transport", "http", "-http-addr", tt.addr}, env(tt.env), io.Discard)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseServeFlags() error = %v, want an error: %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			var logs bytes.Buffer
			logAuth(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
			if got := strings.Contains(logs.String(), "bearer tokens travel unencrypted"); got != tt.wantWarn {
				t.Errorf("plain HTTP warning = %v, want %v; logs:\n%s", got, tt.wantWarn, logs.String())
			}
		})
	}
}

func TestLogAuthStdio(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logAuth(serveConfig{transport: transportStdio, ignoredTokens: envAuthTokens}, slog.New(slog.NewTextHandler(&logs, nil)))
	if !strings.Contains(logs.String(), "Ignoring the auth tokens") || !strings.Contains(logs.String(), "Source="+envAuthTokens) {
		t.Errorf("logs = %s, want a line saying the tokens are ignored", logs.String())
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

	srv := httptest.NewServer(httpHandler(mcpserver.New(noTemporal{}, mcpserver.Options{}), nil, discardLogger()))
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
		done <- serveHTTP(ctx, mcpserver.New(noTemporal{}, mcpserver.Options{}), "127.0.0.1:0", nil, discardLogger())
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

// bearer is an http.RoundTripper that sends a bearer token.
type bearer struct{ token string }

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(req)
}

// TestHTTPHandlerAuth serves the MCP handler with tokens, plus a whoami
// tool that reports the caller from its context, so the test sees what
// a tool handler sees.
func TestHTTPHandlerAuth(t *testing.T) {
	t.Parallel()

	tokens, err := auth.ParseTokenList(auth.Entry("alice", aliceToken))
	if err != nil {
		t.Fatal(err)
	}
	server := mcpserver.New(noTemporal{}, mcpserver.Options{})
	type whoamiOut struct {
		Caller string `json:"caller"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "whoami"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, whoamiOut, error) {
		c, _ := auth.CallerFrom(ctx)
		return nil, whoamiOut{Caller: c.Name}, nil
	})
	var logs syncBuffer
	srv := httptest.NewServer(httpHandler(server, tokens, slog.New(slog.NewTextHandler(&logs, nil))))
	t.Cleanup(srv.Close)

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
	for _, tt := range []struct {
		name, header string
		want         int
	}{
		{name: "no token", want: http.StatusUnauthorized},
		{name: "bad token", header: "Bearer " + bobToken, want: http.StatusUnauthorized},
		{name: "good token", header: "Bearer " + aliceToken, want: http.StatusOK},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+mcpPath, strings.NewReader(initialize))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if tt.header != "" {
			req.Header.Set("Authorization", tt.header)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("%s: status = %d, want %d", tt.name, resp.StatusCode, tt.want)
		}
		if tt.want == http.StatusUnauthorized && !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer ") {
			t.Errorf("%s: WWW-Authenticate = %q, want a Bearer challenge", tt.name, resp.Header.Get("WWW-Authenticate"))
		}
	}

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-host"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + mcpPath,
		HTTPClient: &http.Client{Transport: bearer{aliceToken}},
	}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "whoami"})
	if err != nil || res.IsError {
		t.Fatalf("CallTool(whoami) = %+v, %v", res, err)
	}
	if got, _ := res.StructuredContent.(map[string]any)["caller"].(string); got != "alice" {
		t.Errorf("the tool handler saw caller %q, want alice", got)
	}

	if strings.Contains(logs.String(), aliceToken) || strings.Contains(logs.String(), bobToken) {
		t.Errorf("logs contain a token:\n%s", logs.String())
	}
}

func TestHTTPServerLimitsHeaders(t *testing.T) {
	t.Parallel()

	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	srv := httptest.NewUnstartedServer(ok)
	srv.Config = newHTTPServer(ok, discardLogger())
	srv.Start()
	t.Cleanup(srv.Close)

	for _, tt := range []struct {
		name string
		size int
		want int
	}{
		{name: "small headers", size: 100, want: http.StatusOK},
		{name: "headers over the limit", size: 2 * maxHeaderBytes, want: http.StatusRequestHeaderFieldsTooLarge},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Padding", strings.Repeat("x", tt.size))
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("%s: status = %d, want %d", tt.name, resp.StatusCode, tt.want)
		}
	}
}
