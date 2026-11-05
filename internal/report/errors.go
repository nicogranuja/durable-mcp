package report

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/nicogranuja/durable-mcp/internal/github"
)

// Types of the application errors that the report's activities and
// workflow fail with. Go error values do not survive the trip through
// workflow history, so callers classify a failed report with
// errors.As(err, &appErr) and appErr.Type() instead.
const (
	// ErrorTypeRateLimited means GitHub's rate limit was hit. The activity
	// is retried when GitHub says the limit resets.
	ErrorTypeRateLimited = "RateLimited"

	// ErrorTypeUnavailable means GitHub answered with a 5xx status or the
	// request failed in transit. The activity is retried with exponential
	// backoff.
	ErrorTypeUnavailable = "GitHubUnavailable"

	// ErrorTypeUnauthorized means GitHub rejected the token (401): it is
	// missing, invalid, or expired. Not retried.
	ErrorTypeUnauthorized = "Unauthorized"

	// ErrorTypeForbidden means the token lacks a permission the request
	// needs (403, other than a rate limit). Not retried.
	ErrorTypeForbidden = "Forbidden"

	// ErrorTypeNotFound means the repository does not exist or the token
	// cannot see it (404). Not retried.
	ErrorTypeNotFound = "NotFound"

	// ErrorTypeValidationFailed means GitHub rejected the request
	// parameters (422). Not retried.
	ErrorTypeValidationFailed = "ValidationFailed"

	// ErrorTypeRequestRejected means GitHub answered with another 4xx
	// status, such as 410 for a repository with issues turned off. Not
	// retried.
	ErrorTypeRequestRejected = "RequestRejected"

	// ErrorTypeResponseTooLarge means a page was larger than the client's
	// response limit. Not retried: the same page would be as large again.
	ErrorTypeResponseTooLarge = "ResponseTooLarge"

	// ErrorTypeInvalidRequest means the report request was invalid, such as
	// a malformed repository name or window, and nothing was sent to
	// GitHub. Not retried.
	ErrorTypeInvalidRequest = "InvalidRequest"
)

const (
	// minRetryDelay is the shortest delay before retrying a rate-limited
	// request. A zero NextRetryDelay would mean "use the retry policy".
	minRetryDelay = time.Second

	// Repeated secondary rate limits back off from secondaryBackoffBase,
	// doubling with each attempt, up to secondaryBackoffMax.
	secondaryBackoffBase = time.Minute
	secondaryBackoffMax  = 16 * time.Minute
)

// activityError converts an error from the GitHub client into the error an
// activity returns, which tells Temporal whether and when to retry it:
//
//   - A rate limit is retryable, with NextRetryDelay set from the time
//     GitHub said the limit resets. The server waits that long before the
//     next attempt, durably: no worker holds a slot or a goroutine while it
//     waits, and the wait survives worker restarts.
//   - A 4xx response, an invalid argument, or an oversized page is
//     non-retryable, because repeating the same request cannot succeed.
//   - Anything else, such as a 5xx response or a network error, is
//     retryable with the retry policy's exponential backoff.
func activityError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		// The activity was canceled or timed out, which aborted the
		// request. Return the error as it is: the SDK reports an error
		// wrapping context.Canceled as a cancellation.
		return err
	}

	if rl, ok := errors.AsType[*github.RateLimitError](err); ok {
		delay := rateLimitDelay(rl, activity.GetInfo(ctx).Attempt, time.Now())
		activity.GetLogger(ctx).Info("GitHub rate limit hit; retrying after the delay",
			"Kind", rl.Kind.String(), "Delay", delay)
		return temporal.NewApplicationErrorWithOptions(rl.Error(), ErrorTypeRateLimited, temporal.ApplicationErrorOptions{
			NextRetryDelay: delay,
			// Rate limits are expected in normal operation. A benign error
			// is logged at debug level and left out of failure metrics.
			Category: temporal.ApplicationErrorCategoryBenign,
		})
	}
	if errType, ok := nonRetryableType(err); ok {
		return temporal.NewNonRetryableApplicationError(err.Error(), errType, nil)
	}
	return temporal.NewApplicationError(err.Error(), ErrorTypeUnavailable)
}

// nonRetryableType returns the error type for errors that retrying cannot
// fix.
func nonRetryableType(err error) (string, bool) {
	switch {
	case errors.Is(err, github.ErrUnauthorized):
		return ErrorTypeUnauthorized, true
	case errors.Is(err, github.ErrForbidden):
		return ErrorTypeForbidden, true
	case errors.Is(err, github.ErrNotFound):
		return ErrorTypeNotFound, true
	case errors.Is(err, github.ErrValidation):
		return ErrorTypeValidationFailed, true
	case errors.Is(err, github.ErrInvalidArgument):
		return ErrorTypeInvalidRequest, true
	case errors.Is(err, github.ErrResponseTooLarge):
		return ErrorTypeResponseTooLarge, true
	}
	if apiErr, ok := errors.AsType[*github.APIError](err); ok &&
		apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 && apiErr.StatusCode != http.StatusRequestTimeout {
		return ErrorTypeRequestRejected, true
	}
	return "", false
}

// rateLimitDelay returns how long to wait before retrying after rl, on the
// attempt-th attempt.
//
// The wait comes from rl.RetryAt, which the client measured against
// GitHub's clock. For a secondary rate limit it is at least a backoff that
// doubles with each attempt, because GitHub asks integrations that keep
// hitting secondary limits to wait longer each time, or risk being blocked.
func rateLimitDelay(rl *github.RateLimitError, attempt int32, now time.Time) time.Duration {
	delay := rl.RetryAt.Sub(now)
	if rl.Kind == github.SecondaryRateLimit {
		backoff := secondaryBackoffBase
		for i := int32(1); i < attempt && backoff < secondaryBackoffMax; i++ {
			backoff *= 2
		}
		delay = max(delay, min(backoff, secondaryBackoffMax))
	}
	return max(delay, minRetryDelay)
}
