package mcpserver

import (
	"strings"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// testJobID is a job ID of the right shape.
var testJobID = jobIDForKey("test")

// pendingPage is a pending page activity on its attempt-th attempt after
// failing with an application error of errType, or on its first attempt if
// errType is empty.
func pendingPage(errType string, attempt int32, next time.Time) *workflowpb.PendingActivityInfo {
	a := &workflowpb.PendingActivityInfo{
		ActivityId:      "5",
		ActivityType:    &commonpb.ActivityType{Name: "FetchIssuesPage"},
		State:           enumspb.PENDING_ACTIVITY_STATE_SCHEDULED,
		Attempt:         attempt,
		MaximumAttempts: 10,
	}
	if errType != "" {
		a.LastFailure = &failurepb.Failure{
			Message:    "github: GET /repos/acme/widgets/issues: " + errType,
			StackTrace: "goroutine 1 [running]:\nmain.secret()",
			FailureInfo: &failurepb.Failure_ApplicationFailureInfo{
				ApplicationFailureInfo: &failurepb.ApplicationFailureInfo{Type: errType},
			},
		}
	}
	if !next.IsZero() {
		a.NextAttemptScheduleTime = timestamppb.New(next)
	}
	return a
}

func TestJobStatusRunning(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(23 * time.Minute).UTC().Truncate(time.Second)
	retryAt := time.Now().Add(8 * time.Second).UTC().Truncate(time.Second)
	progress := &report.Progress{Phase: report.PhaseIssues, Pages: report.PageCounts{Issues: 3}}

	tests := []struct {
		name     string
		setup    func(*fakeJob)
		want     []string
		not      []string
		wantStep *StepStatus
	}{
		{
			name: "fetching",
			setup: func(j *fakeJob) {
				j.pending = []*workflowpb.PendingActivityInfo{pendingPage("", 1, time.Time{})}
				j.pending[0].State = enumspb.PENDING_ACTIVITY_STATE_STARTED
			},
			want:     []string{"Running, in phase issues, with 3 issue, 0 pull request, and 0 commit pages fetched so far.", "Check again"},
			not:      []string{"Waiting", "retried"},
			wantStep: &StepStatus{Name: "FetchIssuesPage", State: "running", Attempt: 1, MaxAttempts: 10},
		},
		{
			name: "rate limit wait",
			setup: func(j *fakeJob) {
				j.pending = []*workflowpb.PendingActivityInfo{pendingPage(report.ErrorTypeRateLimited, 2, resetAt)}
			},
			want: []string{
				"Waiting for GitHub rate limit until " + resetAt.Format(time.RFC3339) + " (in 2",
				"the job resumes on its own",
			},
			not: []string{"goroutine", "Check again"},
			wantStep: &StepStatus{
				Name: "FetchIssuesPage", State: "scheduled", Attempt: 2, MaxAttempts: 10, NextAttemptAt: resetAt,
				LastFailure: &Failure{Type: report.ErrorTypeRateLimited, Message: "github: GET /repos/acme/widgets/issues: RateLimited"},
			},
		},
		{
			name: "retry after a server error",
			setup: func(j *fakeJob) {
				j.pending = []*workflowpb.PendingActivityInfo{pendingPage(report.ErrorTypeUnavailable, 3, retryAt)}
			},
			want: []string{
				"FetchIssuesPage failed on attempt 2 and will be retried at " + retryAt.Format(time.RFC3339),
				": github: GET /repos/acme/widgets/issues: Unavailable. The job retries on its own.",
			},
			not: []string{"rate limit"},
		},
		{
			name: "retry running",
			setup: func(j *fakeJob) {
				j.pending = []*workflowpb.PendingActivityInfo{pendingPage(report.ErrorTypeUnavailable, 3, time.Time{})}
				j.pending[0].State = enumspb.PENDING_ACTIVITY_STATE_STARTED
			},
			want: []string{"FetchIssuesPage is on attempt 3 after: github: GET /repos/acme/widgets/issues: Unavailable."},
		},
		{
			name:  "no worker answers",
			setup: func(j *fakeJob) { j.progress = nil },
			want:  []string{"Progress is unavailable because no worker answered", "worker may be down"},
		},
		{
			name:  "cancel requested",
			setup: func(j *fakeJob) { j.cancelRequested = true },
			want:  []string{"Cancellation was requested; the job is stopping."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeClient()
			j := f.add(testJobID, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING)
			j.progress = progress
			j.expiration = j.startTime.Add(JobTimeout)
			tt.setup(j)

			_, out, err := newTools(f).jobStatus(t.Context(), nil, JobInput{JobID: testJobID})
			if err != nil {
				t.Fatalf("jobStatus() error = %v", err)
			}
			if out.Status != statusRunning || !out.StartedAt.Equal(j.startTime) || !out.ClosedAt.IsZero() ||
				!out.TimesOutAt.Equal(j.expiration) {
				t.Errorf("jobStatus() = %s, want running since %v, timing out at %v", mustJSON(out), j.startTime, j.expiration)
			}
			if j.progress != nil && (out.Phase != report.PhaseIssues || out.PagesFetched == nil || out.PagesFetched.Issues != 3) {
				t.Errorf("jobStatus() progress = %q, %+v; want issues, 3 pages", out.Phase, out.PagesFetched)
			}
			if j.progress == nil && (out.Phase != "" || out.PagesFetched != nil) {
				t.Errorf("jobStatus() progress = %q, %+v; want none", out.Phase, out.PagesFetched)
			}
			if out.CancelRequested != j.cancelRequested {
				t.Errorf("cancel requested = %v, want %v", out.CancelRequested, j.cancelRequested)
			}
			for _, w := range tt.want {
				if !strings.Contains(out.Message, w) {
					t.Errorf("message = %q, want it to contain %q", out.Message, w)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(out.Message, n) || strings.Contains(mustJSON(out), n) {
					t.Errorf("result %s contains %q", mustJSON(out), n)
				}
			}
			if tt.wantStep != nil && mustJSON(out.CurrentStep) != mustJSON(tt.wantStep) {
				t.Errorf("current step = %s, want %s", mustJSON(out.CurrentStep), mustJSON(tt.wantStep))
			}
		})
	}
}

func TestJobStatusClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status enumspb.WorkflowExecutionStatus
		want   string
		msg    string
	}{
		{enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, statusCompleted, "Call job_result to get the report"},
		{enumspb.WORKFLOW_EXECUTION_STATUS_FAILED, statusFailed, "Call job_result to see why"},
		{enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED, statusCanceled, "was canceled"},
		{enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED, statusTerminated, "terminated by an operator"},
		{enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT, statusTimedOut, "ran longer than its 24h0m0s limit"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			f := newFakeClient()
			j := f.add(testJobID, tt.status)
			j.closeTime = j.startTime.Add(90 * time.Second)
			j.expiration = j.startTime.Add(JobTimeout)
			// A closed run has no pending activity; this one checks that
			// a stale one would not be reported.
			j.pending = []*workflowpb.PendingActivityInfo{pendingPage("", 1, time.Time{})}

			_, out, err := newTools(f).jobStatus(t.Context(), nil, JobInput{JobID: testJobID})
			if err != nil {
				t.Fatalf("jobStatus() error = %v", err)
			}
			if out.Status != tt.want || !out.ClosedAt.Equal(j.closeTime) || !out.TimesOutAt.IsZero() || out.CurrentStep != nil {
				t.Errorf("jobStatus() = %s, want %s, closed at %v, no deadline or step", mustJSON(out), tt.want, j.closeTime)
			}
			// A query of a closed job makes a worker replay its history.
			if f.queries != 0 || out.Phase != "" || out.PagesFetched != nil {
				t.Errorf("%d progress queries, progress %q, %+v; want no query for a closed job", f.queries, out.Phase, out.PagesFetched)
			}
			if !strings.Contains(out.Message, tt.msg) {
				t.Errorf("message = %q, want it to contain %q", out.Message, tt.msg)
			}
		})
	}
}

func TestJobToolsRejectBadJobs(t *testing.T) {
	t.Parallel()

	otherWorkflow := jobIDForKey("other")
	tests := []struct {
		name  string
		jobID string
		setup func(*fakeClient)
		want  string
	}{
		{name: "malformed ID", jobID: "report-sdk-go", want: `"report-sdk-go" is not a job ID`},
		{name: "unknown job", jobID: testJobID, want: "no job with ID"},
		{
			name:  "not a report",
			jobID: otherWorkflow,
			setup: func(f *fakeClient) {
				f.add(otherWorkflow, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING).workflowType = "Payroll"
			},
			want: "no job with ID",
		},
		{
			name:  "Temporal unavailable",
			jobID: testJobID,
			setup: func(f *fakeClient) { f.describeErr = serviceerror.NewUnavailable("connection refused") },
			want:  "Retry this call in a few seconds",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeClient()
			if tt.setup != nil {
				tt.setup(f)
			}
			tl := newTools(f)
			in := JobInput{JobID: tt.jobID}
			_, _, statusErr := tl.jobStatus(t.Context(), nil, in)
			_, _, resultErr := tl.jobResult(t.Context(), nil, in)
			_, _, cancelErr := tl.cancelJob(t.Context(), nil, in)
			for tool, err := range map[string]error{"job_status": statusErr, "job_result": resultErr, "cancel_job": cancelErr} {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("%s error = %v, want it to contain %q", tool, err, tt.want)
				}
			}
			if len(f.cancels) != 0 {
				t.Errorf("canceled %v, want nothing canceled", f.cancels)
			}
		})
	}
}
