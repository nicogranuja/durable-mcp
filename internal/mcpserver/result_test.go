package mcpserver

import (
	"strings"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/temporal"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// activityFailure returns the error a workflow fails with when an activity
// fails with cause, built the way the SDK builds it from history.
func activityFailure(retryState enumspb.RetryState, cause error) error {
	fc := temporal.GetDefaultFailureConverter()
	return fc.FailureToError(&failurepb.Failure{
		Message: "activity error",
		FailureInfo: &failurepb.Failure_ActivityFailureInfo{ActivityFailureInfo: &failurepb.ActivityFailureInfo{
			ActivityType: &commonpb.ActivityType{Name: "FetchRepository"},
			RetryState:   retryState,
		}},
		Cause: fc.ErrorToFailure(cause),
	})
}

func TestJobResultCompleted(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	j := f.add(testJobID, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED)
	j.result = report.Report{
		Repository: report.Repository{FullName: "acme/widgets", HTMLURL: "https://github.com/acme/widgets"},
		Commits:    report.CommitReport{Total: 12},
	}

	_, out, err := newTools(f).jobResult(t.Context(), nil, JobInput{JobID: testJobID})
	if err != nil {
		t.Fatalf("jobResult() error = %v", err)
	}
	if out.Status != statusCompleted || out.Report == nil || out.Failure != nil {
		t.Fatalf("jobResult() = %s, want a completed report", mustJSON(out))
	}
	if out.Report.Repository.FullName != "acme/widgets" || out.Report.Commits.Total != 12 {
		t.Errorf("report = %s, want the workflow's result", mustJSON(out.Report))
	}
}

func TestJobResultWithoutReport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status    enumspb.WorkflowExecutionStatus
		want      string
		msg       string
		wantFails bool
	}{
		{enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, statusRunning, "The report is not ready yet: the job is still running. Call job_status", false},
		{enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED, statusCanceled, "canceled, so there is no report", false},
		{enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED, statusTerminated, "terminated by an operator", false},
		{enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT, statusTimedOut, "timed out, so there is no report", true},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			f := newFakeClient()
			f.add(testJobID, tt.status)
			_, out, err := newTools(f).jobResult(t.Context(), nil, JobInput{JobID: testJobID})
			if err != nil {
				t.Fatalf("jobResult() error = %v", err)
			}
			if out.Status != tt.want || out.Report != nil || (out.Failure != nil) != tt.wantFails {
				t.Errorf("jobResult() = %s, want status %s, no report", mustJSON(out), tt.want)
			}
			if !strings.Contains(out.Message, tt.msg) {
				t.Errorf("message = %q, want it to contain %q", out.Message, tt.msg)
			}
		})
	}
}

func TestJobResultFailed(t *testing.T) {
	t.Parallel()

	appErr := func(msg, errType string) error {
		return activityFailure(enumspb.RETRY_STATE_NON_RETRYABLE_FAILURE, temporal.NewNonRetryableApplicationError(msg, errType, nil))
	}
	tests := []struct {
		name          string
		err           error
		wantType      string
		wantRetryable bool
		wantMessage   string
		wantNext      string
	}{
		{
			name:     "not found",
			err:      appErr("github: GET /repos/acme/nope: 404 Not Found", report.ErrorTypeNotFound),
			wantType: report.ErrorTypeNotFound, wantMessage: "github: GET /repos/acme/nope: 404 Not Found",
			wantNext: "Check owner and repo; do not retry the same request.",
		},
		{
			name:     "bad token",
			err:      appErr("github: 401 Bad credentials", report.ErrorTypeUnauthorized),
			wantType: report.ErrorTypeUnauthorized, wantNext: "Do not retry: the server's operator must fix GITHUB_TOKEN.",
		},
		{
			name: "rate limit outlasted the budget",
			err: activityFailure(enumspb.RETRY_STATE_TIMEOUT,
				temporal.NewApplicationError("github: rate limit exceeded", report.ErrorTypeRateLimited)),
			wantType: report.ErrorTypeRateLimited, wantRetryable: true, wantNext: "Start a new report later",
		},
		{
			name: "server errors outlasted the retries",
			err: activityFailure(enumspb.RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED,
				temporal.NewApplicationError("github: 502 Bad Gateway", report.ErrorTypeUnavailable)),
			wantType: report.ErrorTypeUnavailable, wantRetryable: true, wantNext: "Start a new report in a few minutes.",
		},
		{
			name:     "invalid request, from the workflow",
			err:      temporal.NewNonRetryableApplicationError("since must be before until", report.ErrorTypeInvalidRequest, nil),
			wantType: report.ErrorTypeInvalidRequest, wantMessage: "since must be before until", wantNext: "Fix the arguments",
		},
		{
			name:     "activity timeout",
			err:      activityFailure(enumspb.RETRY_STATE_TIMEOUT, temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil)),
			wantType: "Timeout", wantRetryable: true, wantNext: "Start a new report in a few minutes.",
		},
		{
			name:     "unknown",
			err:      temporal.NewNonRetryableApplicationError("panic: oops", "", nil),
			wantType: "Unknown", wantMessage: "panic: oops", wantNext: "if it fails again, stop and tell the user",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeClient()
			f.add(testJobID, enumspb.WORKFLOW_EXECUTION_STATUS_FAILED).err = tt.err

			_, out, err := newTools(f).jobResult(t.Context(), nil, JobInput{JobID: testJobID})
			if err != nil {
				t.Fatalf("jobResult() error = %v", err)
			}
			if out.Status != statusFailed || out.Report != nil || out.Failure == nil {
				t.Fatalf("jobResult() = %s, want a failure", mustJSON(out))
			}
			if out.Failure.Type != tt.wantType || out.Failure.Retryable != tt.wantRetryable {
				t.Errorf("failure = %+v, want type %q, retryable %v", out.Failure, tt.wantType, tt.wantRetryable)
			}
			if tt.wantMessage != "" && out.Failure.Message != tt.wantMessage {
				t.Errorf("failure message = %q, want %q", out.Failure.Message, tt.wantMessage)
			}
			if !strings.HasPrefix(out.Message, "The job failed: ") || !strings.Contains(out.Message, tt.wantNext) {
				t.Errorf("message = %q, want the failure and %q", out.Message, tt.wantNext)
			}
		})
	}
}

func TestJobResultTemporalError(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	f.add(testJobID, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED)
	f.getErr = serviceerror.NewUnavailable("connection reset")

	_, out, err := newTools(f).jobResult(t.Context(), nil, JobInput{JobID: testJobID})
	if err == nil || !strings.Contains(err.Error(), "job_result: the job service (Temporal) did not respond") {
		t.Errorf("jobResult() = %s, %v; want a tool error about Temporal", mustJSON(out), err)
	}
}
