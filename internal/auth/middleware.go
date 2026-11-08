package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// realm names the protected resource in WWW-Authenticate challenges.
const realm = "durable-mcp"

// maxTokenLength bounds the token a request may present. Minted tokens are
// 31 characters; anything far longer is not a token.
const maxTokenLength = 256

// rejectionLogInterval is the least time between two logged rejections. A
// client with a stale token, or a scanner, can send many requests a second,
// and each would otherwise write a log line.
const rejectionLogInterval = 10 * time.Second

// Middleware returns a handler that serves next only to requests with a
// valid bearer token in the Authorization header.
//
// Other requests get 401 Unauthorized with a WWW-Authenticate challenge
// (RFC 6750) and a short reason. Rejections are sampled into the log: at
// most one every 10 seconds, with the remote address, the reason, and how
// many rejections were left out since the last line; never the token or
// the header.
//
// An authenticated request reaches next with its [Caller] in the request
// context, and with the caller's name as the MCP SDK's token info
// (auth.TokenInfo.UserID), which the SDK passes to tool handlers in the
// request's Extra field.
func Middleware(tokens *Tokens, logger *slog.Logger, next http.Handler) http.Handler {
	rejections := &rejectionLog{logger: logger, now: time.Now}

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
			rejections.reject(w, r, "missing bearer token", "")
			return
		}
		caller, ok := tokens.Verify(token)
		if !ok {
			rejections.reject(w, r, "invalid bearer token", `, error="invalid_token"`)
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

// rejectionLog answers rejected requests and logs a sample of them.
type rejectionLog struct {
	logger *slog.Logger
	now    func() time.Time

	mu         sync.Mutex
	next       time.Time // when the next rejection may be logged
	suppressed int       // rejections not logged since the last line
}

func (l *rejectionLog) reject(w http.ResponseWriter, r *http.Request, reason, errParam string) {
	l.log(r, reason)
	w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`+errParam)
	http.Error(w, reason+": send Authorization: Bearer TOKEN with a token from the server's operator", http.StatusUnauthorized)
}

func (l *rejectionLog) log(r *http.Request, reason string) {
	l.mu.Lock()
	now := l.now()
	if now.Before(l.next) {
		l.suppressed++
		l.mu.Unlock()
		return
	}
	suppressed := l.suppressed
	l.suppressed = 0
	l.next = now.Add(rejectionLogInterval)
	l.mu.Unlock()

	l.logger.Info("Rejected an HTTP request", "Reason", reason, "RemoteAddr", r.RemoteAddr,
		"Method", r.Method, "Path", r.URL.Path, "SuppressedSinceLast", suppressed)
}
