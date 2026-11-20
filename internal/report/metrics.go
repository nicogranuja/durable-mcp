package report

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/nicogranuja/durable-mcp/internal/github"
)

// Metrics that the workflow and activities record. They go through the
// Temporal SDK's metrics handler, which the worker backs with
// OpenTelemetry, so they are exported next to the SDK's own temporal_*
// metrics. The handler skips workflow metrics while a workflow replays, so
// a replay does not count a job twice.
//
// Every attribute value comes from a fixed set: no job IDs, repositories,
// or callers, which would give each job its own time series.
const (
	// MetricJobsFinished counts reports that ended, by
	// AttrJobOutcome and, for failures, AttrErrorType.
	MetricJobsFinished = "durable_mcp.jobs.finished"

	// MetricRateLimitWait is a histogram, in seconds, of how long a
	// rate-limited GitHub request waits before its retry, by
	// AttrRateLimitKind.
	MetricRateLimitWait = "durable_mcp.github.rate_limit.wait.duration"

	// MetricActivityAttempts counts activity attempts, by
	// AttrActivityName, AttrActivityRetry, and, for failed attempts,
	// AttrErrorType. Temporal retries an activity on the server, so the
	// workflow and its history see only the last attempt; this counts
	// them all.
	MetricActivityAttempts = "durable_mcp.activity.attempts"
)

// Metric attributes.
const (
	// AttrJobOutcome is how a report ended: one of the Outcome* values.
	AttrJobOutcome = "durable_mcp.job.outcome"

	// AttrErrorType is the report error type (such as report.NotFound),
	// ErrorTypeCanceled, ErrorTypeTimeout, or ErrorTypeOther. Successes
	// do not have it.
	AttrErrorType = "error.type"

	// AttrRateLimitKind is "primary" or "secondary".
	AttrRateLimitKind = "durable_mcp.github.rate_limit.kind"

	// AttrActivityName is the activity type name, one of those Register
	// registers, or ErrorTypeOther.
	AttrActivityName = "durable_mcp.activity.name"

	// AttrActivityRetry is "true" for any attempt after the first.
	AttrActivityRetry = "durable_mcp.activity.retry"
)

// Values of AttrJobOutcome.
const (
	OutcomeCompleted = "completed"
	OutcomeFailed    = "failed"
	OutcomeCanceled  = "canceled"
)

// Values of AttrErrorType besides the report error types.
const (
	// ErrorTypeCanceled is an activity attempt that was canceled.
	ErrorTypeCanceled = "canceled"
	// ErrorTypeTimeout is a Temporal timeout, or an attempt whose
	// deadline passed.
	ErrorTypeTimeout = "timeout"
	// ErrorTypeOther is any other error, and any name outside the known
	// set, as in OpenTelemetry's semantic conventions.
	ErrorTypeOther = "_OTHER"
)

// RateLimitWaitBuckets returns the bucket boundaries, in seconds, of
// MetricRateLimitWait: from the one-second minimum, through the backoff
// of repeated secondary limits (1 to 16 minutes), to the hour a primary
// limit can take to reset, and the 24-hour cap.
func RateLimitWaitBuckets() []float64 {
	return []float64{1, 5, 15, 30, 60, 120, 300, 600, 960, 1800, 3600, 7200, 86400}
}

// activityNames are the names AttrActivityName can take: the activity
// names, and the unprefixed names Register also registers.
var activityNames = func() []string {
	names := []string{ActivityFetchRepository, ActivityFetchIssuesPage, ActivityFetchPullRequestsPage, ActivityFetchCommitsPage}
	for _, n := range names[:4] {
		names = append(names, n[len(activityPrefix):])
	}
	return names
}()

// recordFinished counts a report that ended with err. Call it only once
// the workflow function has returned, never from a deferred function: the
// SDK also runs deferred functions when it evicts a workflow from its
// cache, mid-run, and that would count a job that is still running.
func recordFinished(ctx workflow.Context, err error) {
	tags := map[string]string{AttrJobOutcome: OutcomeCompleted}
	switch {
	case err == nil:
	case temporal.IsCanceledError(err):
		tags[AttrJobOutcome] = OutcomeCanceled
	default:
		tags[AttrJobOutcome] = OutcomeFailed
		tags[AttrErrorType] = errorType(err)
	}
	workflow.GetMetricsHandler(ctx).WithTags(tags).Counter(MetricJobsFinished).Inc(1)
}

// recordRateLimitWait records the delay before the retry of a request
// that hit a rate limit.
func recordRateLimitWait(ctx context.Context, kind github.RateLimitKind, delay time.Duration) {
	activity.GetMetricsHandler(ctx).
		WithTags(map[string]string{AttrRateLimitKind: kind.String()}).
		Timer(MetricRateLimitWait).Record(delay)
}

// errorType returns the AttrErrorType value for err.
func errorType(err error) string {
	if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
		if t, ok := NormalizeErrorType(appErr.Type()); ok {
			return t
		}
		return ErrorTypeOther
	}
	var timeoutErr *temporal.TimeoutError
	switch {
	case temporal.IsCanceledError(err):
		return ErrorTypeCanceled
	case errors.As(err, &timeoutErr), errors.Is(err, context.DeadlineExceeded):
		return ErrorTypeTimeout
	}
	return ErrorTypeOther
}

// NewMetricsInterceptor returns a worker interceptor that counts every
// activity attempt in MetricActivityAttempts. Pass it in
// worker.Options.Interceptors.
func NewMetricsInterceptor() interceptor.WorkerInterceptor {
	return &metricsInterceptor{}
}

type metricsInterceptor struct {
	interceptor.WorkerInterceptorBase
}

func (*metricsInterceptor) InterceptActivity(_ context.Context, next interceptor.ActivityInboundInterceptor) interceptor.ActivityInboundInterceptor {
	i := &activityMetrics{}
	i.Next = next
	return i
}

type activityMetrics struct {
	interceptor.ActivityInboundInterceptorBase
}

func (a *activityMetrics) ExecuteActivity(ctx context.Context, in *interceptor.ExecuteActivityInput) (any, error) {
	result, err := a.Next.ExecuteActivity(ctx, in)
	info := activity.GetInfo(ctx)
	name := info.ActivityType.Name
	if !slices.Contains(activityNames, name) {
		name = ErrorTypeOther
	}
	tags := map[string]string{
		AttrActivityName:  name,
		AttrActivityRetry: strconv.FormatBool(info.Attempt > 1),
	}
	if err != nil {
		tags[AttrErrorType] = errorType(err)
	}
	activity.GetMetricsHandler(ctx).WithTags(tags).Counter(MetricActivityAttempts).Inc(1)
	return result, err
}
