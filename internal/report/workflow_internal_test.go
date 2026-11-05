package report

import (
	"errors"
	"strings"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	"go.temporal.io/sdk/temporal"
)

// activityFailure returns the error a workflow sees when an activity fails
// with cause and the given retry state, built the way the SDK builds it from
// history.
func activityFailure(retryState enumspb.RetryState, cause error) error {
	fc := temporal.GetDefaultFailureConverter()
	return fc.FailureToError(&failurepb.Failure{
		Message: "activity error",
		FailureInfo: &failurepb.Failure_ActivityFailureInfo{ActivityFailureInfo: &failurepb.ActivityFailureInfo{
			ActivityType: &commonpb.ActivityType{Name: "FetchIssuesPage"},
			RetryState:   retryState,
		}},
		Cause: fc.ErrorToFailure(cause),
	})
}

func TestFailureMessage(t *testing.T) {
	t.Parallel()

	unavailable := temporal.NewApplicationError("github: GET /repos/acme/widgets/issues: 502 Bad Gateway", ErrorTypeUnavailable)
	notFound := temporal.NewNonRetryableApplicationError("github: GET /repos/acme/nope: 404 Not Found", ErrorTypeNotFound, nil)

	tests := []struct {
		name string
		err  error
		want []string
		not  []string
	}{
		{
			name: "attempts exhausted",
			err:  activityFailure(enumspb.RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED, unavailable),
			want: []string{"502 Bad Gateway", "type: " + ErrorTypeUnavailable, "gave up after the maximum number of attempts"},
			not:  []string{"scheduledEventID"},
		},
		{
			name: "out of time",
			err:  activityFailure(enumspb.RETRY_STATE_TIMEOUT, unavailable),
			want: []string{"502 Bad Gateway", "gave up when the schedule-to-close timeout ran out"},
		},
		{
			name: "non-retryable",
			err:  activityFailure(enumspb.RETRY_STATE_NON_RETRYABLE_FAILURE, notFound),
			want: []string{"404 Not Found", "type: " + ErrorTypeNotFound},
			not:  []string{"gave up"},
		},
		{
			name: "timeout without an application error",
			err:  activityFailure(enumspb.RETRY_STATE_TIMEOUT, temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_START_TO_CLOSE, nil)),
			want: []string{"StartToClose", "gave up when the schedule-to-close timeout ran out"},
		},
		{
			name: "workflow error",
			err:  temporal.NewNonRetryableApplicationError("since must be before until", ErrorTypeInvalidRequest, nil),
			want: []string{"since must be before until", "type: " + ErrorTypeInvalidRequest},
		},
		{name: "other error", err: errors.New("boom"), want: []string{"boom"}},
	}
	for _, tt := range tests {
		got := failureMessage(tt.err)
		for _, w := range tt.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: failureMessage() = %q, want it to contain %q", tt.name, got, w)
			}
		}
		for _, n := range tt.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: failureMessage() = %q, want it not to contain %q", tt.name, got, n)
			}
		}
	}
}
