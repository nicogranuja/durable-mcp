package report

import (
	"testing"
	"time"

	"github.com/nicogranuja/durable-mcp/internal/github"
)

func TestRateLimitDelay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	limit := func(kind github.RateLimitKind, retryIn time.Duration) *github.RateLimitError {
		return &github.RateLimitError{APIError: &github.APIError{StatusCode: 403}, Kind: kind, RetryAt: now.Add(retryIn)}
	}

	tests := []struct {
		name    string
		err     *github.RateLimitError
		attempt int32
		want    time.Duration
	}{
		{"primary waits until the reset", limit(github.PrimaryRateLimit, 42*time.Minute), 1, 42 * time.Minute},
		{"primary does not back off", limit(github.PrimaryRateLimit, 10*time.Second), 6, 10 * time.Second},
		{"reset already passed", limit(github.PrimaryRateLimit, -time.Minute), 1, time.Second},
		{"secondary first attempt", limit(github.SecondaryRateLimit, 30*time.Second), 1, time.Minute},
		{"secondary second attempt", limit(github.SecondaryRateLimit, time.Minute), 2, 2 * time.Minute},
		{"secondary fourth attempt", limit(github.SecondaryRateLimit, time.Minute), 4, 8 * time.Minute},
		{"secondary backoff is capped", limit(github.SecondaryRateLimit, time.Minute), 30, 16 * time.Minute},
		{"secondary Retry-After beyond the backoff", limit(github.SecondaryRateLimit, 20*time.Minute), 2, 20 * time.Minute},
	}
	for _, tt := range tests {
		if got := rateLimitDelay(tt.err, tt.attempt, now); got != tt.want {
			t.Errorf("%s: rateLimitDelay() = %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestNormalizeErrorType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
		ok       bool
	}{
		{ErrorTypeNotFound, ErrorTypeNotFound, true},
		{"NotFound", ErrorTypeNotFound, true}, // a run from before the prefix
		{"RateLimited", ErrorTypeRateLimited, true},
		{"report.Bogus", "", false},
		{"", "", false},
		{"*errors.errorString", "", false},
	}
	for _, tt := range tests {
		if got, ok := NormalizeErrorType(tt.in); got != tt.want || ok != tt.ok {
			t.Errorf("NormalizeErrorType(%q) = %q, %v, want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
