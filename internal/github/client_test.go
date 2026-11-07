package github_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicogranuja/durable-mcp/internal/github"
)

const testToken = "test-token"

// newTestServer starts an httptest server and returns a client pointed at it.
func newTestServer(t *testing.T, handler http.HandlerFunc, opts ...github.Option) (*github.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	opts = append([]github.Option{github.WithBaseURL(srv.URL)}, opts...)
	c, err := github.New(testToken, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

// newTestClient is newTestServer for tests that don't need the server.
func newTestClient(t *testing.T, handler http.HandlerFunc, opts ...github.Option) *github.Client {
	t.Helper()
	c, _ := newTestServer(t, handler, opts...)
	return c
}

// respond returns a handler that writes a fixed status, headers, and body.
func respond(status int, body string, headerKV ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headerKV); i += 2 {
			w.Header().Set(headerKV[i], headerKV[i+1])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

const repoJSON = `{
	"id": 1296269,
	"name": "widgets",
	"full_name": "acme/widgets",
	"owner": {"id": 7, "login": "acme", "type": "Organization"},
	"description": null,
	"html_url": "https://github.com/acme/widgets",
	"default_branch": "main",
	"language": "Go",
	"archived": false,
	"stargazers_count": 42,
	"forks_count": 3,
	"open_issues_count": 5,
	"created_at": "2020-01-02T03:04:05Z",
	"updated_at": "2026-10-30T12:00:00Z",
	"pushed_at": null
}`

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		token   string
		opts    []github.Option
		wantErr string
	}{
		{name: "defaults", token: testToken},
		{name: "unauthenticated", token: ""},
		{name: "enterprise base URL", token: testToken, opts: []github.Option{github.WithBaseURL("https://ghe.example.com/api/v3")}},
		{name: "plain HTTP to loopback", token: testToken, opts: []github.Option{github.WithBaseURL("http://127.0.0.1:8080")}},
		{name: "plain HTTP to localhost", token: testToken, opts: []github.Option{github.WithBaseURL("http://localhost:8080/")}},
		{name: "plain HTTP without a token", token: "", opts: []github.Option{github.WithBaseURL("http://ghe.example.com/api/v3")}},
		{name: "zero timeout disables it", token: testToken, opts: []github.Option{github.WithTimeout(0)}},
		{
			name: "plain HTTP with a token", token: testToken,
			opts:    []github.Option{github.WithBaseURL("http://ghe.example.com/api/v3")},
			wantErr: "refusing to send a token over plain HTTP",
		},
		{name: "relative base URL", token: testToken, opts: []github.Option{github.WithBaseURL("api/v3")}, wantErr: "absolute http(s) URL"},
		{name: "unsupported scheme", token: testToken, opts: []github.Option{github.WithBaseURL("ftp://example.com/")}, wantErr: "absolute http(s) URL"},
		{name: "credentials in base URL", token: testToken, opts: []github.Option{github.WithBaseURL("https://u:p@example.com/")}, wantErr: "must not contain credentials"},
		{name: "query in base URL", token: testToken, opts: []github.Option{github.WithBaseURL("https://example.com/?x=1")}, wantErr: "must not contain credentials, a query"},
		{name: "negative timeout", token: testToken, opts: []github.Option{github.WithTimeout(-time.Second)}, wantErr: "timeout must not be negative"},
		{name: "zero response limit", token: testToken, opts: []github.Option{github.WithMaxResponseBytes(0)}, wantErr: "max response bytes must be positive"},
		{name: "empty user agent", token: testToken, opts: []github.Option{github.WithUserAgent("")}, wantErr: "user agent must not be empty"},
		{name: "empty API version", token: testToken, opts: []github.Option{github.WithAPIVersion("")}, wantErr: "API version must not be empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, err := github.New(tt.token, tt.opts...)
			if tt.wantErr == "" {
				if err != nil || c == nil {
					t.Fatalf("New() = %v, %v; want a client", c, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("New() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestRequestHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		token          string
		opts           []github.Option
		wantAuth       string
		wantUserAgent  string
		wantAPIVersion string
	}{
		{
			name:           "defaults with token",
			token:          "s3cret",
			wantAuth:       "Bearer s3cret",
			wantUserAgent:  github.DefaultUserAgent,
			wantAPIVersion: github.DefaultAPIVersion,
		},
		{
			name:           "no token sends no Authorization header",
			token:          "",
			wantAuth:       "",
			wantUserAgent:  github.DefaultUserAgent,
			wantAPIVersion: github.DefaultAPIVersion,
		},
		{
			name:           "overrides",
			token:          "s3cret",
			opts:           []github.Option{github.WithUserAgent("report-bot/1.0"), github.WithAPIVersion("2022-11-28")},
			wantAuth:       "Bearer s3cret",
			wantUserAgent:  "report-bot/1.0",
			wantAPIVersion: "2022-11-28",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := make(chan http.Header, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headers <- r.Header.Clone()
				respond(http.StatusOK, repoJSON)(w, r)
			}))
			t.Cleanup(srv.Close)

			c, err := github.New(tt.token, append(tt.opts, github.WithBaseURL(srv.URL))...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := c.GetRepository(t.Context(), "acme", "widgets"); err != nil {
				t.Fatalf("GetRepository: %v", err)
			}
			got := <-headers

			checks := []struct{ header, want string }{
				{"Authorization", tt.wantAuth},
				{"Accept", "application/vnd.github+json"},
				{"X-GitHub-Api-Version", tt.wantAPIVersion},
				{"User-Agent", tt.wantUserAgent},
			}
			for _, h := range checks {
				if v := got.Get(h.header); v != h.want {
					t.Errorf("%s = %q, want %q", h.header, v, h.want)
				}
			}
		})
	}
}

func TestGetRepository(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		basePath string
		wantPath string
	}{
		{name: "github.com", basePath: "", wantPath: "/repos/acme/widgets"},
		{name: "enterprise server", basePath: "/api/v3", wantPath: "/api/v3/repos/acme/widgets"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			paths := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths <- r.URL.Path
				respond(http.StatusOK, repoJSON)(w, r)
			}))
			t.Cleanup(srv.Close)
			c, err := github.New(testToken, github.WithBaseURL(srv.URL+tt.basePath))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			repo, err := c.GetRepository(t.Context(), "acme", "widgets")
			if err != nil {
				t.Fatalf("GetRepository: %v", err)
			}
			if gotPath := <-paths; gotPath != tt.wantPath {
				t.Errorf("request path = %q, want %q", gotPath, tt.wantPath)
			}
			if repo.FullName != "acme/widgets" || repo.Owner.Login != "acme" || repo.StargazersCount != 42 {
				t.Errorf("decoded repository = %+v", repo)
			}
			if want := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC); !repo.CreatedAt.Equal(want) {
				t.Errorf("CreatedAt = %v, want %v", repo.CreatedAt, want)
			}
			if !repo.PushedAt.IsZero() || repo.Description != "" {
				t.Errorf("null fields should decode to zero values, got PushedAt=%v Description=%q", repo.PushedAt, repo.Description)
			}
		})
	}
}

func TestGetRepositoryInvalidArguments(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		respond(http.StatusOK, repoJSON)(w, r)
	})

	tests := []struct{ owner, repo string }{
		{"", "widgets"},
		{"acme", ""},
		{"..", "widgets"},
		{"acme", ".."},
		{"acme/../../user", "widgets"},
		{"acme", "widgets/issues"},
		{"acme", "wid%2Fgets"},
		{"acme", "widgets?x=1"},
		{"acme", strings.Repeat("a", 101)},
	}
	for _, tt := range tests {
		t.Run(tt.owner+"/"+tt.repo, func(t *testing.T) {
			t.Parallel()
			_, err := c.GetRepository(t.Context(), tt.owner, tt.repo)
			if !errors.Is(err, github.ErrInvalidArgument) {
				t.Fatalf("error = %v, want ErrInvalidArgument", err)
			}
		})
	}
	t.Cleanup(func() {
		if n := hits.Load(); n != 0 {
			t.Errorf("server received %d requests, want 0", n)
		}
	})
}

func TestAPIErrors(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		github.ErrUnauthorized,
		github.ErrForbidden,
		github.ErrNotFound,
		github.ErrValidation,
		github.ErrServer,
	}

	tests := []struct {
		name        string
		status      int
		body        string
		wantIs      error // nil means no sentinel should match
		wantMessage string
		wantText    string // substring of err.Error()
	}{
		{
			name:        "unauthorized",
			status:      http.StatusUnauthorized,
			body:        `{"message":"Bad credentials","documentation_url":"https://docs.github.com/rest"}`,
			wantIs:      github.ErrUnauthorized,
			wantMessage: "Bad credentials",
			wantText:    "github: GET /repos/acme/widgets: 401 Bad credentials",
		},
		{
			name:        "forbidden without rate-limit headers",
			status:      http.StatusForbidden,
			body:        `{"message":"Resource not accessible by personal access token"}`,
			wantIs:      github.ErrForbidden,
			wantMessage: "Resource not accessible by personal access token",
		},
		{
			name:        "not found",
			status:      http.StatusNotFound,
			body:        `{"message":"Not Found"}`,
			wantIs:      github.ErrNotFound,
			wantMessage: "Not Found",
			wantText:    "404 Not Found",
		},
		{
			name:        "validation failed with field errors",
			status:      http.StatusUnprocessableEntity,
			body:        `{"message":"Validation Failed","errors":[{"resource":"Issue","field":"since","code":"invalid"},"per_page is too large"]}`,
			wantIs:      github.ErrValidation,
			wantMessage: "Validation Failed",
			wantText:    "422 Validation Failed (Issue.since: invalid; per_page is too large)",
		},
		{
			name:        "server error",
			status:      http.StatusInternalServerError,
			body:        `{"message":"Server Error"}`,
			wantIs:      github.ErrServer,
			wantMessage: "Server Error",
		},
		{
			name:     "bad gateway with an HTML body",
			status:   http.StatusBadGateway,
			body:     `<html><body>502 Bad Gateway</body></html>`,
			wantIs:   github.ErrServer,
			wantText: "502 Bad Gateway",
		},
		{
			// Error bodies have their own 64 KiB read limit, separate from
			// the 1 KiB WithMaxResponseBytes used in this test.
			name:        "error body over the response limit is still decoded",
			status:      http.StatusUnprocessableEntity,
			body:        `{"message":"` + strings.Repeat("x", 8<<10) + `"}`,
			wantIs:      github.ErrValidation,
			wantMessage: strings.Repeat("x", 8<<10),
		},
		{
			name:     "error body over the error read limit is truncated",
			status:   http.StatusServiceUnavailable,
			body:     `{"message":"` + strings.Repeat("x", 200<<10) + `"}`,
			wantIs:   github.ErrServer,
			wantText: "503 Service Unavailable",
		},
		{
			name:        "conflict maps to no sentinel",
			status:      http.StatusConflict,
			body:        `{"message":"Git Repository is empty."}`,
			wantMessage: "Git Repository is empty.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t,
				respond(tt.status, tt.body, "X-GitHub-Request-Id", "ABCD:1234"),
				github.WithMaxResponseBytes(1<<10),
			)

			_, err := c.GetRepository(t.Context(), "acme", "widgets")

			var apiErr *github.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v (%T), want *github.APIError", err, err)
			}
			if apiErr.StatusCode != tt.status || apiErr.Method != http.MethodGet || apiErr.Path != "/repos/acme/widgets" {
				t.Errorf("APIError = %d %s %s, want %d GET /repos/acme/widgets",
					apiErr.StatusCode, apiErr.Method, apiErr.Path, tt.status)
			}
			if apiErr.RequestID != "ABCD:1234" {
				t.Errorf("RequestID = %q, want %q", apiErr.RequestID, "ABCD:1234")
			}
			if tt.wantMessage != "" && apiErr.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", apiErr.Message, tt.wantMessage)
			}
			if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("Error() = %q, want it to contain %q", err.Error(), tt.wantText)
			}
			for _, s := range sentinels {
				if got, want := errors.Is(err, s), errors.Is(s, tt.wantIs); got != want {
					t.Errorf("errors.Is(err, %v) = %t, want %t", s, got, want)
				}
			}
		})
	}
}

func TestMalformedJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		body   string
		wantAs func(error) bool
	}{
		{
			name: "truncated body",
			body: `{"id": 1, "name": "wid`,
			wantAs: func(err error) bool {
				var target *json.SyntaxError
				return errors.As(err, &target)
			},
		},
		{
			name: "not JSON",
			body: `<html>captive portal</html>`,
			wantAs: func(err error) bool {
				var target *json.SyntaxError
				return errors.As(err, &target)
			},
		},
		{
			name: "wrong field type",
			body: `{"id": "not-a-number"}`,
			wantAs: func(err error) bool {
				var target *json.UnmarshalTypeError
				return errors.As(err, &target)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, respond(http.StatusOK, tt.body))

			_, err := c.GetRepository(t.Context(), "acme", "widgets")
			if err == nil {
				t.Fatal("GetRepository succeeded, want a decode error")
			}
			if !tt.wantAs(err) {
				t.Errorf("error = %v (%T), want it to wrap the JSON decode error", err, err)
			}
			if !strings.Contains(err.Error(), "decode response") {
				t.Errorf("error = %q, want it to mention decoding", err)
			}
		})
	}
}

func TestResponseSizeLimit(t *testing.T) {
	t.Parallel()

	limit := int64(len(repoJSON))
	tests := []struct {
		name    string
		limit   int64
		wantErr error
	}{
		{name: "body exactly at the limit", limit: limit},
		{name: "body one byte over the limit", limit: limit - 1, wantErr: github.ErrResponseTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, respond(http.StatusOK, repoJSON), github.WithMaxResponseBytes(tt.limit))

			_, err := c.GetRepository(t.Context(), "acme", "widgets")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCancellationAndTimeouts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []github.Option
		// ctx returns the context for the request. arrived is closed when
		// the server has received the request.
		ctx         func(t *testing.T, arrived <-chan struct{}) context.Context
		want        error
		wantNoCalls bool
	}{
		{
			name: "caller cancels while the request is in flight",
			ctx: func(t *testing.T, arrived <-chan struct{}) context.Context {
				t.Helper()
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				go func() {
					<-arrived
					cancel()
				}()
				return ctx
			},
			want: context.Canceled,
		},
		{
			name: "caller context already canceled",
			ctx: func(t *testing.T, _ <-chan struct{}) context.Context {
				t.Helper()
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			want:        context.Canceled,
			wantNoCalls: true,
		},
		{
			name: "caller deadline expires",
			ctx: func(t *testing.T, _ <-chan struct{}) context.Context {
				t.Helper()
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			},
			want: context.DeadlineExceeded,
		},
		{
			name: "client request timeout expires",
			opts: []github.Option{github.WithTimeout(50 * time.Millisecond)},
			ctx: func(t *testing.T, _ <-chan struct{}) context.Context {
				t.Helper()
				return t.Context()
			},
			want: context.DeadlineExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			arrived := make(chan struct{})
			var calls atomic.Int32
			// The handler blocks until the client gives up, so the only way
			// for the request to finish is through the context.
			c := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(arrived)
				}
				<-r.Context().Done()
			}, tt.opts...)

			start := time.Now()
			_, err := c.GetRepository(tt.ctx(t, arrived), "acme", "widgets")
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("request took %s to fail; the context was not honored", elapsed)
			}
			if tt.wantNoCalls && calls.Load() != 0 {
				t.Errorf("server received %d requests, want 0", calls.Load())
			}
		})
	}
}

func TestRedirects(t *testing.T) {
	t.Parallel()

	// outside stands in for any server the token must never reach.
	var outsideHits atomic.Int32
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outsideHits.Add(1)
		respond(http.StatusOK, repoJSON)(w, r)
	}))
	t.Cleanup(outside.Close)

	tests := []struct {
		name     string
		basePath string
		tls      bool
		// location returns the redirect target given the API server's URL.
		location func(api string) string
		// wantErr is a substring of the expected error; "" means the
		// redirect should be followed.
		wantErr string
	}{
		{
			name:     "renamed repository on the same API",
			location: func(api string) string { return api + "/repositories/42" },
		},
		{
			name:     "another host",
			location: func(string) string { return outside.URL + "/repos/acme/widgets" },
			wantErr:  "refusing to follow redirect",
		},
		{
			name:     "HTTPS API redirecting to plain HTTP",
			tls:      true,
			location: func(string) string { return outside.URL + "/repos/acme/widgets" },
			wantErr:  "refusing to follow redirect",
		},
		{
			name:     "same host outside the enterprise API path",
			basePath: "/api/v3",
			location: func(api string) string { return api + "/login/oauth" },
			wantErr:  "refusing to follow redirect",
		},
		{
			name:     "encoded dot segments",
			basePath: "/api/v3",
			location: func(api string) string { return api + "/api/v3/%2e%2e/login/oauth" },
			wantErr:  "refusing to follow redirect",
		},
		{
			name:     "redirect loop",
			location: func(api string) string { return api + "/repos/acme/widgets" },
			wantErr:  "stopped after 10 redirects",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// followed receives the Authorization header of every request
			// that arrives after a redirect.
			followed := make(chan string, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tt.basePath+"/repos/acme/widgets" {
					api := "http://" + r.Host
					if r.TLS != nil {
						api = "https://" + r.Host
					}
					http.Redirect(w, r, tt.location(api), http.StatusMovedPermanently)
					return
				}
				followed <- r.Header.Get("Authorization")
				respond(http.StatusOK, repoJSON)(w, r)
			})
			srv := httptest.NewUnstartedServer(handler)
			if tt.tls {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			t.Cleanup(srv.Close)

			c, err := github.New(testToken, github.WithBaseURL(srv.URL+tt.basePath), github.WithHTTPClient(srv.Client()))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = c.GetRepository(t.Context(), "acme", "widgets")

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("GetRepository error = %v, want it to contain %q", err, tt.wantErr)
				}
				if !errors.Is(err, github.ErrRedirectRefused) {
					t.Errorf("GetRepository error = %v, want it to match ErrRedirectRefused", err)
				}
				select {
				case <-followed:
					t.Error("the API server received the redirected request")
				default:
				}
				return
			}
			if err != nil {
				t.Fatalf("GetRepository: %v", err)
			}
			if auth := <-followed; auth != "Bearer "+testToken {
				t.Errorf("redirected request Authorization = %q, want the token", auth)
			}
		})
	}

	t.Cleanup(func() {
		if n := outsideHits.Load(); n != 0 {
			t.Errorf("outside server received %d requests, want 0", n)
		}
	})
}

func TestWithHTTPClientKeepsCallerRedirectPolicy(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repositories/42", http.StatusMovedPermanently)
	}))
	t.Cleanup(srv.Close)

	var calls atomic.Int32
	stop := func(*http.Request, []*http.Request) error {
		calls.Add(1)
		return http.ErrUseLastResponse
	}
	hc := &http.Client{CheckRedirect: stop}
	c, err := github.New(testToken, github.WithBaseURL(srv.URL), github.WithHTTPClient(hc))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The caller's policy still runs after the client's own check: here it
	// stops at the 301, which surfaces as an APIError.
	_, err = c.GetRepository(t.Context(), "acme", "widgets")
	if apiErr, ok := errors.AsType[*github.APIError](err); !ok || apiErr.StatusCode != http.StatusMovedPermanently {
		t.Errorf("GetRepository error = %v, want a 301 APIError", err)
	}
	if calls.Load() != 1 {
		t.Errorf("caller's CheckRedirect ran %d times, want 1", calls.Load())
	}

	// New works on a copy: the caller's client must still allow a redirect
	// to another host, which the client's own check would refuse.
	elsewhere := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://elsewhere.example/", http.NoBody)
	if err := hc.CheckRedirect(elsewhere, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("caller's CheckRedirect returned %v after New, want its own result; New must not modify it", err)
	}
}

func TestClientDoesNotPrintToken(t *testing.T) {
	t.Parallel()

	const secret = "ghp_supersecret"
	c, err := github.New(secret)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("client", "client", c, "value", *c)

	outputs := map[string]string{
		"%v":       fmt.Sprintf("%v", c),
		"%+v":      fmt.Sprintf("%+v", c),
		"%#v":      fmt.Sprintf("%#v", c),
		"%+v of *": fmt.Sprintf("%+v", *c),
		"slog":     logged.String(),
	}
	for format, out := range outputs {
		if strings.Contains(out, "supersecret") {
			t.Errorf("%s output contains the token: %s", format, out)
		}
	}
}

func TestTimeoutWhileReadingBody(t *testing.T) {
	t.Parallel()

	// The server sends the status and part of the body, then stalls. The
	// per-request timeout covers reading the body, not just the headers.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id": 1, "name": "wid`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, github.WithTimeout(50*time.Millisecond))

	_, err := c.GetRepository(t.Context(), "acme", "widgets")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "read response") {
		t.Errorf("error = %q, want it to say the body read failed", err)
	}
}
