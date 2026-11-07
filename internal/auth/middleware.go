package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// realm names the protected resource in WWW-Authenticate challenges.
const realm = "durable-mcp"

// maxTokenLength bounds the token a request may present. Minted tokens are
// 31 characters; anything far longer is not a token.
const maxTokenLength = 256

// Middleware returns a handler that serves next only to requests with a
// valid bearer token in the Authorization header.
//
// Other requests get 401 Unauthorized with a WWW-Authenticate challenge
// (RFC 6750) and a short reason, and are logged with the remote address
// and the reason, never the token or the header.
//
// An authenticated request reaches next with its [Caller] in the request
// context, and with the caller's name as the MCP SDK's token info
// (auth.TokenInfo.UserID), which the SDK passes to tool handlers in the
// request's Extra field.
func Middleware(tokens *Tokens, logger *slog.Logger, next http.Handler) http.Handler {
	// The SDK's own middleware is the way to give the SDK token info. The
	// token was verified before it runs, so it only reads the caller back.
	withTokenInfo := mcpauth.RequireBearerToken(func(ctx context.Context, _ string, _ *http.Request) (*mcpauth.TokenInfo, error) {
		c, ok := CallerFrom(ctx)
		if !ok {
			return nil, mcpauth.ErrInvalidToken
		}
		return &mcpauth.TokenInfo{UserID: c.Name}, nil
	}, &mcpauth.RequireBearerTokenOptions{AllowMissingExpiration: true})(next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			reject(w, r, logger, "missing bearer token", "")
			return
		}
		caller, ok := tokens.Verify(token)
		if !ok {
			reject(w, r, logger, "invalid bearer token", `, error="invalid_token"`)
			return
		}
		withTokenInfo.ServeHTTP(w, r.WithContext(WithCaller(r.Context(), caller)))
	})
}

// bearerToken returns the token of a "Bearer TOKEN" Authorization header.
// The scheme is case-insensitive (RFC 9110, section 11.1).
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxTokenLength || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

func reject(w http.ResponseWriter, r *http.Request, logger *slog.Logger, reason, errParam string) {
	logger.Info("Rejected an HTTP request", "Reason", reason, "RemoteAddr", r.RemoteAddr, "Method", r.Method, "Path", r.URL.Path)
	w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`+errParam)
	http.Error(w, reason+": send Authorization: Bearer TOKEN with a token from the server's operator", http.StatusUnauthorized)
}
