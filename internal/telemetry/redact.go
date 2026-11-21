package telemetry

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// redacted replaces a value that the span policy keeps out of traces.
const redacted = "REDACTED"

// allowedQueryKeys are the query parameters whose values spans keep: the
// ones the GitHub client sends, none of which carries a credential. Every
// other parameter keeps its name, so a trace still shows the request's
// shape, but its value becomes [redacted]. An allowlist, not a denylist:
// a credential in a parameter nobody thought of stays out too.
var allowedQueryKeys = []string{"page", "per_page", "state", "since", "until", "sort", "direction", "sha"}

// The patterns of credentials that free text, such as an error message,
// may quote.
var (
	// urlInText is a URL with a query string, as an error from net/http
	// quotes it: Get "https://host/path?query": ....
	urlInText = regexp.MustCompile(`https?://[^\s"'<>?#]+\?[^\s"'<>#]*`)
	// bearerInText is an Authorization header value.
	bearerInText = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	// knownTokens are the token formats of GitHub and of durable-mcp's own
	// auth tokens (auth.TokenPrefix).
	knownTokens = regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|dmcp_[a-z0-9]{16,})\b`)
)

// redactingExporter applies the span policy to every span before next
// exports it, whatever the instrumentation that recorded it:
//
//   - attributes that hold HTTP headers are dropped, so that turning on
//     header capture somewhere cannot export an Authorization header;
//   - url.full and url.query keep only the values of [allowedQueryKeys];
//   - in every other string, of the span name, attributes, status, events,
//     and links, URLs get the same query policy and credential-shaped
//     substrings become [redacted].
//
// The instrumentation records no credentials to begin with (otelhttp
// captures no headers, and the tool-call spans no arguments); this is the
// backstop for what a dependency, or a future change, might record.
type redactingExporter struct {
	next sdktrace.SpanExporter
}

// ExportSpans exports the redacted copies of spans.
func (e redactingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = redactSpan(s)
	}
	return e.next.ExportSpans(ctx, out)
}

// Shutdown shuts the next exporter down.
func (e redactingExporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }

func redactSpan(s sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	stub := tracetest.SpanStubFromReadOnlySpan(s)
	stub.Name = redactText(stub.Name)
	stub.Attributes = redactAttributes(stub.Attributes)
	stub.Status.Description = redactText(stub.Status.Description)
	for i := range stub.Events {
		stub.Events[i].Name = redactText(stub.Events[i].Name)
		stub.Events[i].Attributes = redactAttributes(stub.Events[i].Attributes)
	}
	for i := range stub.Links {
		stub.Links[i].Attributes = redactAttributes(stub.Links[i].Attributes)
	}
	return stub.Snapshot()
}

func redactAttributes(attrs []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, kv := range attrs {
		key := string(kv.Key)
		switch {
		case isHeaderAttribute(key):
			continue
		case kv.Value.Type() == attribute.STRING:
			out = append(out, attribute.String(key, redactValue(key, kv.Value.AsString())))
		case kv.Value.Type() == attribute.STRINGSLICE:
			vals := slices.Clone(kv.Value.AsStringSlice())
			for i := range vals {
				vals[i] = redactText(vals[i])
			}
			out = append(out, attribute.StringSlice(key, vals))
		default:
			out = append(out, kv)
		}
	}
	return out
}

// isHeaderAttribute reports whether key holds an HTTP header, which the
// semantic conventions name http.request.header.NAME and
// http.response.header.NAME.
func isHeaderAttribute(key string) bool {
	key = strings.ToLower(key)
	return strings.HasPrefix(key, "http.request.header.") || strings.HasPrefix(key, "http.response.header.") ||
		strings.Contains(key, "authorization") || strings.Contains(key, "cookie")
}

func redactValue(key, v string) string {
	switch attribute.Key(key) {
	case semconv.URLFullKey:
		return redactURL(v)
	case semconv.URLQueryKey:
		return redactQuery(v)
	}
	return redactText(v)
}

// redactURL applies the query policy to a URL, and drops any user info.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		// Not a URL that can be checked, so none of it is kept.
		return redacted
	}
	u.User = nil
	u.RawQuery = redactQuery(u.RawQuery)
	u.Fragment, u.RawFragment = "", ""
	return redactCredentials(u.String())
}

// redactQuery keeps the names of a query's parameters, and the values of
// the allowed ones.
func redactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return redacted
	}
	for k, vs := range q {
		if !slices.Contains(allowedQueryKeys, k) {
			for i := range vs {
				vs[i] = redacted
			}
		}
	}
	return q.Encode()
}

// redactText applies the policy to free text: the query policy to the
// URLs in it, then [redactCredentials].
func redactText(s string) string {
	return redactCredentials(urlInText.ReplaceAllStringFunc(s, redactURL))
}

// redactCredentials replaces credential-shaped substrings.
func redactCredentials(s string) string {
	s = bearerInText.ReplaceAllString(s, "$1 "+redacted)
	return knownTokens.ReplaceAllString(s, redacted)
}
