package github_test

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nicogranuja/durable-mcp/internal/github"
)

const (
	primaryLimitBody   = `{"message":"API rate limit exceeded for user ID 1.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api"}`
	secondaryLimitBody = `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`
)

func TestRateLimitErrors(t *testing.T) {
	t.Parallel()

	now := time.Now()
	reset := now.Add(42 * time.Minute).Truncate(time.Second)
	resetUnix := strconv.FormatInt(reset.Unix(), 10)
	retryDate := now.Add(2 * time.Minute).UTC().Truncate(time.Second)
	unix := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	httpDate := func(t time.Time) string { return t.UTC().Format(http.TimeFormat) }
	// GitHub's clock, ten minutes behind and ahead of the local one.
	serverBehind := now.Add(-10 * time.Minute).Truncate(time.Second)
	serverAhead := now.Add(10 * time.Minute).Truncate(time.Second)

	tests := []struct {
		name     string
		status   int
		headers  []string
		body     string
		wantKind github.RateLimitKind
		// Exactly one of wantRetryAt (absolute, to within the one-second
		// resolution of the Date header) and wantRetryIn (relative to the
		// request) is set.
		wantRetryAt time.Time
		wantRetryIn time.Duration
		wantRate    github.Rate
	}{
		{
			name:   "primary limit on 403",
			status: http.StatusForbidden,
			headers: []string{
				"X-RateLimit-Limit", "5000",
				"X-RateLimit-Remaining", "0",
				"X-RateLimit-Used", "5000",
				"X-RateLimit-Reset", resetUnix,
				"X-RateLimit-Resource", "core",
			},
			body:        primaryLimitBody,
			wantKind:    github.PrimaryRateLimit,
			wantRetryAt: reset,
			wantRate:    github.Rate{Limit: 5000, Remaining: 0, Used: 5000, Reset: reset, Resource: "core"},
		},
		{
			name:        "primary limit on 429",
			status:      http.StatusTooManyRequests,
			headers:     []string{"X-RateLimit-Limit", "60", "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", resetUnix},
			body:        primaryLimitBody,
			wantKind:    github.PrimaryRateLimit,
			wantRetryAt: reset,
			wantRate:    github.Rate{Limit: 60, Remaining: 0, Reset: reset},
		},
		{
			name:        "primary limit with a longer Retry-After",
			status:      http.StatusForbidden,
			headers:     []string{"X-RateLimit-Remaining", "0", "X-RateLimit-Reset", resetUnix, "Retry-After", "3600"},
			body:        primaryLimitBody,
			wantKind:    github.PrimaryRateLimit,
			wantRetryIn: time.Hour,
			wantRate:    github.Rate{Remaining: 0, Reset: reset},
		},
		{
			name:        "primary limit without a reset header",
			status:      http.StatusForbidden,
			headers:     []string{"X-RateLimit-Remaining", "0"},
			body:        primaryLimitBody,
			wantKind:    github.PrimaryRateLimit,
			wantRetryIn: time.Minute,
		},
		{
			name:        "primary limit with GitHub's clock behind",
			status:      http.StatusForbidden,
			headers:     []string{"X-RateLimit-Remaining", "0", "X-RateLimit-Reset", unix(serverBehind.Add(5 * time.Minute)), "Date", httpDate(serverBehind)},
			body:        primaryLimitBody,
			wantKind:    github.PrimaryRateLimit,
			wantRetryIn: 5 * time.Minute,
			wantRate:    github.Rate{Remaining: 0, Reset: serverBehind.Add(5 * time.Minute)},
		},
		{
			name:        "primary limit with GitHub's clock ahead",
			status:      http.StatusForbidden,
			headers:     []string{"X-RateLimit-Remaining", "0", "X-RateLimit-Reset", unix(serverAhead.Add(5 * time.Minute)), "Date", httpDate(serverAhead)},
			body:        primaryLimitBody,
			wantKind:    github.PrimaryRateLimit,
			wantRetryIn: 5 * time.Minute,
			wantRate:    github.Rate{Remaining: 0, Reset: serverAhead.Add(5 * time.Minute)},
		},
		{
			name:        "primary limit whose reset has already passed",
			status:      http.StatusForbidden,
			headers:     []string{"X-RateLimit-Remaining", "0", "X-RateLimit-Reset", unix(serverBehind.Add(-time.Minute)), "Date", httpDate(serverBehind)},
			body:        primaryLimitBody,
			wantKind:    github.PrimaryRateLimit,
			wantRetryIn: 0,
			wantRate:    github.Rate{Remaining: 0, Reset: serverBehind.Add(-time.Minute)},
		},
		{
			name:        "bogus reset far in the future is capped",
			status:      http.StatusForbidden,
			headers:     []string{"X-RateLimit-Remaining", "0", "X-RateLimit-Reset", unix(now.AddDate(1, 0, 0)), "Date", httpDate(now)},
			body:        primaryLimitBody,
			wantKind:    github.PrimaryRateLimit,
			wantRetryIn: 24 * time.Hour,
			wantRate:    github.Rate{Remaining: 0, Reset: now.AddDate(1, 0, 0).Truncate(time.Second)},
		},
		{
			name:        "secondary limit with an exhausted quota",
			status:      http.StatusForbidden,
			headers:     []string{"X-RateLimit-Remaining", "0", "X-RateLimit-Reset", resetUnix},
			body:        secondaryLimitBody,
			wantKind:    github.SecondaryRateLimit,
			wantRetryAt: reset,
			wantRate:    github.Rate{Remaining: 0, Reset: reset},
		},
		{
			name:        "secondary limit with Retry-After seconds",
			status:      http.StatusForbidden,
			headers:     []string{"Retry-After", "30", "X-RateLimit-Limit", "5000", "X-RateLimit-Remaining", "4870", "X-RateLimit-Reset", resetUnix},
			body:        secondaryLimitBody,
			wantKind:    github.SecondaryRateLimit,
			wantRetryIn: 30 * time.Second,
			wantRate:    github.Rate{Limit: 5000, Remaining: 4870, Reset: reset},
		},
		{
			name:        "secondary limit with a Retry-After date",
			status:      http.StatusTooManyRequests,
			headers:     []string{"Retry-After", retryDate.Format(http.TimeFormat)},
			body:        secondaryLimitBody,
			wantKind:    github.SecondaryRateLimit,
			wantRetryAt: retryDate,
		},
		{
			name:        "secondary limit identified by message only",
			status:      http.StatusForbidden,
			headers:     []string{"X-RateLimit-Remaining", "4999"},
			body:        secondaryLimitBody,
			wantKind:    github.SecondaryRateLimit,
			wantRetryIn: time.Minute,
			wantRate:    github.Rate{Remaining: 4999},
		},
		{
			name:        "429 without any rate-limit headers",
			status:      http.StatusTooManyRequests,
			body:        `{"message":"Too Many Requests"}`,
			wantKind:    github.SecondaryRateLimit,
			wantRetryIn: time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, respond(tt.status, tt.body, tt.headers...))

			before := time.Now()
			_, err := c.GetRepository(t.Context(), "acme", "widgets")
			after := time.Now()

			var rl *github.RateLimitError
			if !errors.As(err, &rl) {
				t.Fatalf("error = %v (%T), want *github.RateLimitError", err, err)
			}
			if !errors.Is(err, github.ErrRateLimited) {
				t.Error("errors.Is(err, ErrRateLimited) = false, want true")
			}
			if errors.Is(err, github.ErrForbidden) {
				t.Error("errors.Is(err, ErrForbidden) = true; a rate limit must not look like a permission error")
			}
			if _, ok := errors.AsType[*github.APIError](err); ok {
				t.Error("errors.As(err, *APIError) = true; rate limits should only be *RateLimitError")
			}

			if rl.Kind != tt.wantKind {
				t.Errorf("Kind = %v, want %v", rl.Kind, tt.wantKind)
			}
			if rl.StatusCode != tt.status || rl.Path != "/repos/acme/widgets" {
				t.Errorf("response = %d %s, want %d /repos/acme/widgets", rl.StatusCode, rl.Path, tt.status)
			}
			if !rateEqual(rl.Rate, tt.wantRate) {
				t.Errorf("Rate = %+v, want %+v", rl.Rate, tt.wantRate)
			}

			if !tt.wantRetryAt.IsZero() {
				lo, hi := tt.wantRetryAt.Add(-time.Second), tt.wantRetryAt.Add(time.Second+after.Sub(before))
				if rl.RetryAt.Before(lo) || rl.RetryAt.After(hi) {
					t.Errorf("RetryAt = %v, want %v (to within a second)", rl.RetryAt, tt.wantRetryAt)
				}
			} else {
				// RetryAt is computed from the client's clock when the
				// response arrives, so bound it by the request window.
				lo, hi := before.Add(tt.wantRetryIn), after.Add(tt.wantRetryIn)
				if rl.RetryAt.Before(lo) || rl.RetryAt.After(hi) {
					t.Errorf("RetryAt = %v, want between %v and %v", rl.RetryAt, lo, hi)
				}
			}

			wantText := strconv.Itoa(tt.status) + " " + tt.wantKind.String() + " rate limit exceeded, retry at "
			if !strings.Contains(err.Error(), wantText) {
				t.Errorf("Error() = %q, want it to contain %q", err.Error(), wantText)
			}
		})
	}
}

func TestForbiddenIsNotARateLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headers []string
	}{
		{name: "quota remaining", headers: []string{"X-RateLimit-Limit", "5000", "X-RateLimit-Remaining", "4999"}},
		{name: "unparseable Retry-After", headers: []string{"Retry-After", "soon"}},
		{name: "no headers"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := `{"message":"Resource not accessible by personal access token"}`
			c := newTestClient(t, respond(http.StatusForbidden, body, tt.headers...))

			_, err := c.GetRepository(t.Context(), "acme", "widgets")
			if !errors.Is(err, github.ErrForbidden) {
				t.Errorf("error = %v, want ErrForbidden", err)
			}
			if errors.Is(err, github.ErrRateLimited) {
				t.Errorf("error = %v matches ErrRateLimited, want a plain permission error", err)
			}
		})
	}
}

// rateEqual compares Rates, using time.Time.Equal for Reset.
func rateEqual(a, b github.Rate) bool {
	return a.Limit == b.Limit && a.Remaining == b.Remaining && a.Used == b.Used &&
		a.Resource == b.Resource && a.Reset.Equal(b.Reset)
}
