package github

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// secondaryRetryFallback is how long to wait after a secondary rate limit
// when GitHub sends no Retry-After header. GitHub's guidance is to wait at
// least one minute.
const secondaryRetryFallback = time.Minute

// maxRetryAfter caps any rate-limit wait so a bogus Retry-After or
// X-RateLimit-Reset header cannot overflow time.Duration or park a caller
// for days.
const maxRetryAfter = 24 * time.Hour

// RateLimitKind distinguishes GitHub's two kinds of rate limit.
type RateLimitKind int

const (
	// PrimaryRateLimit is the per-token request quota reported in the
	// X-RateLimit-* headers. It resets at a fixed time, at most an hour
	// away.
	PrimaryRateLimit RateLimitKind = iota + 1

	// SecondaryRateLimit protects GitHub from bursts: too many concurrent
	// requests, too many requests to one endpoint per minute, or too much
	// compute time. GitHub may say when to retry with Retry-After. Repeated
	// violations can get an integration blocked, so callers should back off
	// further each time they hit one.
	SecondaryRateLimit
)

// String returns "primary" or "secondary".
func (k RateLimitKind) String() string {
	switch k {
	case PrimaryRateLimit:
		return "primary"
	case SecondaryRateLimit:
		return "secondary"
	default:
		return "unknown"
	}
}

// Rate is the primary rate-limit state from the X-RateLimit-* response
// headers.
type Rate struct {
	// Limit is the number of requests allowed in the current window.
	Limit int
	// Remaining is the number of requests left in the current window.
	Remaining int
	// Used is the number of requests made in the current window.
	Used int
	// Reset is when the current window ends and Remaining is refilled.
	Reset time.Time
	// Resource is the quota bucket, such as "core" or "search".
	Resource string
}

// parseRate reads the X-RateLimit-* headers. It reports false if the
// response carries no rate-limit information.
func parseRate(h http.Header) (Rate, bool) {
	remaining, err := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if err != nil {
		return Rate{}, false
	}
	r := Rate{Remaining: remaining, Resource: h.Get("X-RateLimit-Resource")}
	// The remaining headers are informational; a malformed one leaves the
	// field zero rather than discarding the rest.
	r.Limit, _ = strconv.Atoi(h.Get("X-RateLimit-Limit"))
	r.Used, _ = strconv.Atoi(h.Get("X-RateLimit-Used"))
	if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		r.Reset = time.Unix(reset, 0)
	}
	return r, true
}

// RateLimitError is returned when GitHub rejects a request because of a
// primary or secondary rate limit. The client does not wait or retry; the
// caller decides what to do with RetryAt. See the package documentation for
// why.
//
// It matches ErrRateLimited with errors.Is and deliberately does not match
// ErrForbidden, even when the status is 403, so a rate limit is never
// mistaken for a permission problem.
type RateLimitError struct {
	// APIError is the rejected response, with status 403 or 429.
	*APIError

	// Kind says which rate limit was hit.
	Kind RateLimitKind

	// RetryAt is the earliest time, on the local clock, that a retry can
	// succeed: the response's arrival time plus the longest wait GitHub
	// asked for, through X-RateLimit-Reset (measured against GitHub's Date
	// header, so a skewed local clock does not shorten or stretch it) and
	// Retry-After. Without either, the wait is one minute. It is capped at
	// 24 hours.
	RetryAt time.Time

	// Rate is the primary rate-limit state, if the response included it.
	Rate Rate
}

// Error implements the error interface.
func (e *RateLimitError) Error() string {
	return fmt.Sprintf("github: %s %s: %d %s rate limit exceeded, retry at %s",
		e.Method, e.Path, e.StatusCode, e.Kind, e.RetryAt.UTC().Format(time.RFC3339))
}

// Is reports whether target is ErrRateLimited.
func (e *RateLimitError) Is(target error) bool {
	return target == ErrRateLimited
}

// responseError converts a non-2xx response into a *RateLimitError or an
// *APIError.
func responseError(resp *http.Response, body []byte, now time.Time) error {
	apiErr := newAPIError(resp, body)
	if rl := rateLimitError(apiErr, resp.Header, now); rl != nil {
		return rl
	}
	return apiErr
}

// rateLimitError reports whether a 403 or 429 response is a rate limit, and
// if so which kind and when to retry, following GitHub's documented rules:
//
//   - A message that mentions a secondary rate limit means a secondary
//     limit, whatever the headers say.
//   - Otherwise X-RateLimit-Remaining: 0 means the primary quota is
//     exhausted until X-RateLimit-Reset.
//   - Otherwise Retry-After, or a 429 status, means a secondary limit.
//
// Any other 403 is a permission error and rateLimitError returns nil.
//
// The wait is the longest of the signals present: the time until
// X-RateLimit-Reset when the quota is exhausted, and Retry-After. With
// neither, GitHub's guidance is to wait at least a minute.
func rateLimitError(apiErr *APIError, h http.Header, now time.Time) *RateLimitError {
	if apiErr.StatusCode != http.StatusForbidden && apiErr.StatusCode != http.StatusTooManyRequests {
		return nil
	}

	rate, hasRate := parseRate(h)
	exhausted := hasRate && rate.Remaining == 0
	retryAfter, hasRetryAfter := parseRetryAfter(h.Get("Retry-After"), now)
	secondary := strings.Contains(strings.ToLower(apiErr.Message), "secondary rate limit")

	e := &RateLimitError{APIError: apiErr, Rate: rate}
	switch {
	case secondary:
		e.Kind = SecondaryRateLimit
	case exhausted:
		e.Kind = PrimaryRateLimit
	case hasRetryAfter || apiErr.StatusCode == http.StatusTooManyRequests:
		e.Kind = SecondaryRateLimit
	default:
		return nil
	}

	var wait time.Duration
	known := false
	if exhausted && !rate.Reset.IsZero() {
		wait, known = untilReset(rate.Reset, h.Get("Date"), now), true
	}
	if hasRetryAfter {
		wait, known = max(wait, retryAfter), true
	}
	if !known {
		wait = secondaryRetryFallback
	}
	e.RetryAt = now.Add(min(wait, maxRetryAfter))
	return e
}

// untilReset returns how long until reset. It measures against GitHub's
// clock, from the response's Date header, when that is available: reset is
// an absolute time on GitHub's clock, and comparing it with a local clock
// that runs fast would retry while still limited, which GitHub warns can get
// an integration blocked.
func untilReset(reset time.Time, date string, now time.Time) time.Duration {
	serverNow := now
	if t, err := http.ParseTime(date); err == nil {
		serverNow = t
	}
	return max(reset.Sub(serverNow), 0)
}

// parseRetryAfter parses a Retry-After value, which is either a number of
// seconds or an HTTP date (RFC 9110, section 10.2.3). GitHub sends seconds.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		secs = min(secs, int64(maxRetryAfter/time.Second))
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(t.Sub(now), 0), maxRetryAfter), true
	}
	return 0, false
}
