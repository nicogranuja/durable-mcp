package report

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/nicogranuja/durable-mcp/internal/github"
)

// Types of the application errors that the report's activities and
// workflow fail with, such as "report.NotFound". Go error values do not
// survive the trip through workflow history, so callers classify a failed
// report with errors.As(err, &appErr) and appErr.Type() instead.
//
// The "report." prefix, which activity names share, keeps the types apart
// from other workers' on a shared task queue. Runs started before the
// prefix have unprefixed types in their failures; [NormalizeErrorType] maps
// them to the current ones.
const (
	// ErrorTypeRateLimited means GitHub's rate limit was hit. The activity
	// is retried when GitHub says the limit resets.
	ErrorTypeRateLimited = "report.RateLimited"

	// ErrorTypeUnavailable means GitHub answered with a 5xx status or the
	// request failed in transit. The activity is retried with exponential
	// backoff.
	ErrorTypeUnavailable = "report.Unavailable"

	// ErrorTypeUnauthorized means GitHub rejected the token (401): it is
	// missing, invalid, or expired. Not retried.
	ErrorTypeUnauthorized = "report.Unauthorized"

	// ErrorTypeForbidden means the token lacks a permission the request
	// needs (403, other than a rate limit). Not retried.
	ErrorTypeForbidden = "report.Forbidden"

	// ErrorTypeNotFound means the repository does not exist or the token
	// cannot see it (404). Not retried.
	ErrorTypeNotFound = "report.NotFound"

	// ErrorTypeValidationFailed means GitHub rejected the request
	// parameters (422). Not retried.
	ErrorTypeValidationFailed = "report.ValidationFailed"

	// ErrorTypeRequestRejected means GitHub answered with another 4xx
	// status, such as 451 for a repository blocked for legal reasons. Not
	// retried. 408 Request Timeout is the exception: it is retried as
	// ErrorTypeUnavailable.
	ErrorTypeRequestRejected = "report.RequestRejected"

	// ErrorTypeResponseTooLarge means a page was larger than the client's
	// response limit. Not retried: the same page would be as large again.
	ErrorTypeResponseTooLarge = "report.ResponseTooLarge"

	// ErrorTypeInvalidRequest means the report request was invalid, such as
	// a malformed repository name or window, and nothing was sent to
	// GitHub. Not retried.
	ErrorTypeInvalidRequest = "report.InvalidRequest"

	// ErrorTypeLinkRefused means GitHub's answer pointed outside the
	// configured API, through a next-page link or a redirect, and the client
	// refused to follow it so that the token stays on the API. It usually
	// means the worker's GitHub API URL is wrong, or a proxy rewrites links.
	// Not retried: the same request gets the same answer.
	ErrorTypeLinkRefused = "report.LinkRefused"

	// ErrorTypeInternal means the worker could not make the request at all:
	// it could not build it, or a page cursor from the workflow's history
	// does not belong to the worker's GitHub API URL, as after that URL
	// changed mid-report. It is a bug or a configuration change, not
	// something the request or GitHub can fix. Not retried.
	ErrorTypeInternal = "report.Internal"
)

// errorTypes lists every report error type.
var errorTypes = []string{
	ErrorTypeRateLimited, ErrorTypeUnavailable, ErrorTypeUnauthorized, ErrorTypeForbidden, ErrorTypeNotFound,
	ErrorTypeValidationFailed, ErrorTypeRequestRejected, ErrorTypeResponseTooLarge, ErrorTypeInvalidRequest,
	ErrorTypeLinkRefused, ErrorTypeInternal,
}

// ErrorTypes returns every report error type.
func ErrorTypes() []string { return slices.Clone(errorTypes) }

// NormalizeErrorType returns the report error type that t names, accepting
// the unprefixed types of runs that started before the prefix, and reports
// whether t is a report error type at all.
func NormalizeErrorType(t string) (string, bool) {
	if !strings.HasPrefix(t, activityPrefix) {
		t = activityPrefix + t
	}
	if slices.Contains(errorTypes, t) {
		return t, true
	}
	return "", false
}

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
//   - A 4xx response other than 408, an invalid argument, an oversized
//     page, a next-page link or redirect off the API, or a request that
//     cannot be built is non-retryable, because repeating the same request
//     cannot succeed.
//   - Anything else, such as a 5xx or 408 response, a network error, the
//     client's own request timeout, or a body that is not the JSON
//     expected, is retryable with the retry policy's exponential backoff:
//     another attempt can get a different answer.
func activityError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		// The activity's context ended, which aborted the request: Temporal
		// canceled the activity, its timeout passed, or the worker is
		// stopping. Report the context's cause, so that the SDK reports a
		// cancellation by Temporal (a *temporal.CanceledError cause) as a
		// cancellation, whatever the aborted request returned.
		if cause := context.Cause(ctx); !errors.Is(err, cause) {
			return fmt.Errorf("%w: %w", cause, err)
		}
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
	// Before ErrInvalidArgument, which it also matches: a cursor comes from
	// the workflow's history, not from the request.
	case errors.Is(err, github.ErrInvalidCursor), errors.Is(err, github.ErrBuildRequest), errors.Is(err, github.ErrPageLimit):
		return ErrorTypeInternal, true
	case errors.Is(err, github.ErrInvalidNextLink), errors.Is(err, github.ErrRedirectRefused):
		return ErrorTypeLinkRefused, true
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
// doubles with each attempt at the same page, because GitHub asks
// integrations that keep hitting secondary limits to wait longer each time,
// or risk being blocked.
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
