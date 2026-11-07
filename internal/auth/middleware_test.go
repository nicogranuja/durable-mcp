package auth_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/nicogranuja/durable-mcp/internal/auth"
)

func TestMiddleware(t *testing.T) {
	t.Parallel()

	tokens := mustTokens(t, auth.Entry("alice", aliceToken))
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	h := auth.Middleware(tokens, logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := auth.CallerFrom(r.Context())
		info := mcpauth.TokenInfoFromContext(r.Context())
		if !ok || info == nil || info.UserID != c.Name {
			t.Errorf("handler got caller %+v, %v and token info %+v, want both", c, ok, info)
		}
		w.Write([]byte("hello " + c.Name))
	}))

	tests := []struct {
		name, header   string
		wantStatus     int
		wantChallenge  string
		wantBodyPrefix string
	}{
		{"valid", "Bearer " + aliceToken, http.StatusOK, "", "hello alice"},
		{"scheme in lower case", "bearer " + aliceToken, http.StatusOK, "", "hello alice"},
		{"missing", "", http.StatusUnauthorized, `Bearer realm="durable-mcp"`, "missing bearer token"},
		{"basic auth", "Basic YWxpY2U6c2VjcmV0", http.StatusUnauthorized, `Bearer realm="durable-mcp"`, "missing bearer token"},
		{"empty token", "Bearer ", http.StatusUnauthorized, `Bearer realm="durable-mcp"`, "missing bearer token"},
		{"wrong token", "Bearer " + bobToken, http.StatusUnauthorized, `Bearer realm="durable-mcp", error="invalid_token"`, "invalid bearer token"},
		{"two tokens", "Bearer " + aliceToken + " " + aliceToken, http.StatusUnauthorized, `Bearer realm="durable-mcp"`, "missing bearer token"},
		{"huge token", "Bearer " + strings.Repeat("x", 10_000), http.StatusUnauthorized, `Bearer realm="durable-mcp"`, "missing bearer token"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if tt.header != "" {
			req.Header.Set("Authorization", tt.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.wantStatus {
			t.Errorf("%s: status = %d, want %d", tt.name, rec.Code, tt.wantStatus)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != tt.wantChallenge {
			t.Errorf("%s: WWW-Authenticate = %q, want %q", tt.name, got, tt.wantChallenge)
		}
		if !strings.HasPrefix(rec.Body.String(), tt.wantBodyPrefix) {
			t.Errorf("%s: body = %q, want it to start with %q", tt.name, rec.Body.String(), tt.wantBodyPrefix)
		}
	}

	// Rejections are logged without the token.
	for _, secret := range []string{aliceToken, bobToken, "YWxpY2U6c2VjcmV0"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain a presented credential:\n%s", logs.String())
		}
	}
	if !strings.Contains(logs.String(), "Reason=\"invalid bearer token\"") {
		t.Errorf("logs = %s, want the rejection reason", logs.String())
	}
}
