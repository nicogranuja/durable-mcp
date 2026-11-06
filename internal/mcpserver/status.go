package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	workflowpb "go.temporal.io/api/workflow/v1"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// Job states, from the workflow execution status.
const (
	statusRunning    = "running"
	statusCompleted  = "completed"
	statusFailed     = "failed"
	statusCanceled   = "canceled"
	statusTerminated = "terminated"
	statusTimedOut   = "timed_out"
	statusPaused     = "paused"
	statusUnknown    = "unknown"
)

// runStatus names a workflow execution status as a job state.
func runStatus(s enumspb.WorkflowExecutionStatus) string {
	switch s {
	case enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING:
		return statusRunning
	case enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		return statusCompleted
	case enumspb.WORKFLOW_EXECUTION_STATUS_FAILED:
		return statusFailed
	case enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED:
		return statusCanceled
	case enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED:
		return statusTerminated
	case enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
		return statusTimedOut
	case enumspb.WORKFLOW_EXECUTION_STATUS_PAUSED:
		return statusPaused
	default:
		// Continued-as-new does not happen: the workflow never continues as
		// new, and describing a job ID returns its latest run.
		return statusUnknown
	}
}

var jobStatusTool = &mcp.Tool{
	Name:  "job_status",
	Title: "Check a report job",
	Description: `Check on a job that start_report started: its state (running, completed, failed, canceled, terminated, timed_out),
which phase it is in and how many pages it has fetched, and what it is doing right now, including retries and waits.

A job waiting for a GitHub rate limit says until when; it resumes on its own, so there is nothing to do but check back after that time.
Polling every 10 to 60 seconds is plenty. Once the state is completed, call job_result for the report;
for failed, job_result explains why.`,
	Annotations: &mcp.ToolAnnotations{
		Title:          "Check a report job",
		ReadOnlyHint:   true,
		IdempotentHint: true,
		OpenWorldHint:  new(false),
	},
}

// JobInput is the input of the tools that take a job ID.
type JobInput struct {
	JobID string `json:"job_id" jsonschema:"The job_id that start_report returned, such as report-mfrggzdfmztwq2lknnwg23tpoa."`
}

// JobStatusOutput is the result of job_status.
type JobStatusOutput struct {
	JobID  string `json:"job_id"`
	Status string `json:"status" jsonschema:"The job's state: running, completed, failed, canceled, terminated, timed_out, or paused."`

	Phase        report.Phase       `json:"phase,omitempty" jsonschema:"While the job runs, the report's phase: started, repository, issues, pull_requests, or commits. Missing once it finished, or if no worker answered."`
	PagesFetched *report.PageCounts `json:"pages_fetched,omitempty" jsonschema:"While the job runs, the pages fetched so far, per resource. Missing once it finished, or if no worker answered."`

	CancelRequested bool      `json:"cancel_requested,omitempty" jsonschema:"True once cancel_job was called for a job that is still stopping."`
	StartedAt       time.Time `json:"started_at,omitzero"`
	ClosedAt        time.Time `json:"closed_at,omitzero" jsonschema:"When the job finished. Missing while it runs."`
	TimesOutAt      time.Time `json:"times_out_at,omitzero" jsonschema:"When a job that is still running will be stopped as timed_out."`

	CurrentStep *StepStatus `json:"current_step,omitempty" jsonschema:"The step the job is running or waiting to retry. Missing between steps and after the job finishes."`

	Message string `json:"message" jsonschema:"A one-line summary of the job's state and what to do next."`
}

// StepStatus describes the activity that a running job is on.
type StepStatus struct {
	Name          string    `json:"name" jsonschema:"The step, such as FetchIssuesPage."`
	State         string    `json:"state" jsonschema:"scheduled (waiting to run or to retry), running, cancel_requested, or paused."`
	Attempt       int32     `json:"attempt" jsonschema:"The current attempt, from 1."`
	MaxAttempts   int32     `json:"max_attempts,omitempty"`
	LastFailure   *Failure  `json:"last_failure,omitempty" jsonschema:"Why the previous attempt failed, if it did."`
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero" jsonschema:"When the step will be retried, if it is waiting to retry."`
}

// Failure is an error from a job or one of its steps.
type Failure struct {
	Type    string `json:"type,omitempty" jsonschema:"The error type, such as RateLimited, Unavailable, or NotFound."`
	Message string `json:"message"`
}

func (t *tools) jobStatus(ctx context.Context, _ *mcp.CallToolRequest, in JobInput) (*mcp.CallToolResult, JobStatusOutput, error) {
	resp, err := t.describeJob(ctx, "job_status", in.JobID)
	if err != nil {
		return nil, JobStatusOutput{}, err
	}
	info := resp.GetWorkflowExecutionInfo()
	out := JobStatusOutput{
		JobID:           in.JobID,
		Status:          runStatus(info.GetStatus()),
		CancelRequested: resp.GetWorkflowExtendedInfo().GetCancelRequested() && info.GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
		StartedAt:       timestamp(info.GetStartTime().AsTime(), info.GetStartTime() != nil),
		ClosedAt:        timestamp(info.GetCloseTime().AsTime(), info.GetCloseTime() != nil),
	}
	if exp := resp.GetWorkflowExtendedInfo().GetExecutionExpirationTime(); exp != nil && out.Status == statusRunning {
		out.TimesOutAt = timestamp(exp.AsTime(), true)
	}
	if out.Status == statusRunning || out.Status == statusPaused {
		if pending := resp.GetPendingActivities(); len(pending) > 0 {
			// The workflow runs one activity at a time.
			out.CurrentStep = stepStatus(pending[0])
		}
	}

	// Only an open job is queried. A query of a closed one makes a worker
	// replay its whole history, on every poll, for nothing the answer
	// needs: the state says how it ended.
	haveProgress := false
	if out.Status == statusRunning || out.Status == statusPaused {
		progress, err := t.progress(ctx, in.JobID, info.GetExecution().GetRunId())
		if err == nil {
			out.Phase = progress.Phase
			out.PagesFetched = &progress.Pages
			haveProgress = true
		} else {
			t.logger.Info("Progress query failed", "JobID", in.JobID, "Error", err)
		}
	}
	out.Message = statusMessage(out, haveProgress, time.Now())
	return nil, out, nil
}

// progress runs the progress query against a run of the job.
func (t *tools) progress(ctx context.Context, jobID, runID string) (report.Progress, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	v, err := t.client.QueryWorkflow(ctx, jobID, runID, report.ProgressQuery)
	if err != nil {
		return report.Progress{}, err
	}
	var p report.Progress
	if err := v.Get(&p); err != nil {
		return report.Progress{}, fmt.Errorf("decode progress: %w", err)
	}
	return p, nil
}

// stepStatus describes a pending activity.
func stepStatus(a *workflowpb.PendingActivityInfo) *StepStatus {
	s := &StepStatus{
		Name:        a.GetActivityType().GetName(),
		Attempt:     a.GetAttempt(),
		MaxAttempts: a.GetMaximumAttempts(),
		LastFailure: failureOf(a.GetLastFailure()),
	}
	switch a.GetState() {
	case enumspb.PENDING_ACTIVITY_STATE_SCHEDULED:
		s.State = "scheduled"
	case enumspb.PENDING_ACTIVITY_STATE_STARTED:
		s.State = "running"
	case enumspb.PENDING_ACTIVITY_STATE_CANCEL_REQUESTED:
		s.State = "cancel_requested"
	case enumspb.PENDING_ACTIVITY_STATE_PAUSED, enumspb.PENDING_ACTIVITY_STATE_PAUSE_REQUESTED:
		s.State = "paused"
	default:
		s.State = statusUnknown
	}
	if next := a.GetNextAttemptScheduleTime(); next != nil {
		s.NextAttemptAt = timestamp(next.AsTime(), true)
	}
	return s
}

// failureOf describes a failure from Temporal: the innermost application
// error's type and message, which say why GitHub or the activity failed.
// Stack traces are left out.
func failureOf(f *failurepb.Failure) *Failure {
	if f == nil {
		return nil
	}
	for c := f; c != nil; c = c.GetCause() {
		if app := c.GetApplicationFailureInfo(); app != nil {
			return &Failure{Type: app.GetType(), Message: truncate(c.GetMessage(), maxMessageLength)}
		}
		if to := c.GetTimeoutFailureInfo(); to != nil {
			return &Failure{Type: "Timeout", Message: truncate(c.GetMessage(), maxMessageLength)}
		}
	}
	return &Failure{Message: truncate(f.GetMessage(), maxMessageLength)}
}

// maxMessageLength caps error messages passed on to the agent.
const maxMessageLength = 500

// statusMessage summarizes a job's state for the agent, with the next
// thing to do.
func statusMessage(out JobStatusOutput, haveProgress bool, now time.Time) string {
	switch out.Status {
	case statusCompleted:
		return "The job completed. Call job_result to get the report."
	case statusFailed:
		return "The job failed. Call job_result to see why and whether starting a new report could help."
	case statusCanceled:
		return "The job was canceled. Start a new report if you still need one."
	case statusTerminated:
		return "The job was terminated by an operator. Start a new report if you still need one."
	case statusTimedOut:
		return fmt.Sprintf("The job timed out: it ran longer than its %s limit. Start a new report with a shorter window if you still need one.", JobTimeout)
	case statusPaused:
		return "The job is paused by an operator. Check again later."
	case statusRunning:
	default:
		return "The job is in an unexpected state. Check again later."
	}

	var b strings.Builder
	if out.CancelRequested {
		b.WriteString("Cancellation was requested; the job is stopping. ")
	}
	if haveProgress {
		p := out.PagesFetched
		fmt.Fprintf(&b, "Running, in phase %s, with %d issue, %d pull request, and %d commit pages fetched so far.",
			out.Phase, p.Issues, p.PullRequests, p.Commits)
	} else {
		b.WriteString("Running. Progress is unavailable because no worker answered; " +
			"if that persists, the durable-mcp worker may be down, and the job will continue when it is back.")
	}

	if s := out.CurrentStep; s != nil && !s.NextAttemptAt.IsZero() && s.NextAttemptAt.After(now) {
		wait := s.NextAttemptAt.Sub(now).Round(time.Second)
		if s.LastFailure != nil && s.LastFailure.Type == report.ErrorTypeRateLimited {
			fmt.Fprintf(&b, " Waiting for GitHub rate limit until %s (in %s); the job resumes on its own, so check back after that.",
				s.NextAttemptAt.Format(time.RFC3339), wait)
		} else {
			fmt.Fprintf(&b, " %s failed on attempt %d and will be retried at %s (in %s)",
				s.Name, s.Attempt-1, s.NextAttemptAt.Format(time.RFC3339), wait)
			if s.LastFailure != nil {
				fmt.Fprintf(&b, ": %s", s.LastFailure.Message)
			}
			b.WriteString(". The job retries on its own.")
		}
		return b.String()
	}
	if s := out.CurrentStep; s != nil && s.Attempt > 1 && s.LastFailure != nil {
		fmt.Fprintf(&b, " %s is on attempt %d after: %s.", s.Name, s.Attempt, s.LastFailure.Message)
	}
	b.WriteString(" Check again in a little while.")
	return b.String()
}

// timestamp returns t in UTC, truncated to the second, or the zero time if
// ok is false. A nil protobuf timestamp converts to the Unix epoch, not the
// zero time, so callers pass whether it was set.
func timestamp(t time.Time, ok bool) time.Time {
	if !ok {
		return time.Time{}
	}
	return t.UTC().Truncate(time.Second)
}
