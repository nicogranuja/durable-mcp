package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

var jobResultTool = &mcp.Tool{
	Name:  "job_result",
	Title: "Get a report job's result",
	Description: `Get the result of a job that start_report started. It never waits for the job.

If the job completed, the result has the report. If it is still running, the result says so (status running, no report):
call job_status to follow it and job_result again once it completes. If it failed, the result explains why
(failure.type is RateLimited, Unavailable, Unauthorized, Forbidden, NotFound, ValidationFailed, RequestRejected,
ResponseTooLarge, InvalidRequest, Timeout, or Unknown) and failure.retryable says whether starting a new report later could succeed.
A canceled, terminated, or timed-out job has no report.`,
	Annotations: &mcp.ToolAnnotations{
		Title:          "Get a report job's result",
		ReadOnlyHint:   true,
		IdempotentHint: true,
		OpenWorldHint:  new(false),
	},
}

// JobResultOutput is the result of job_result.
type JobResultOutput struct {
	JobID   string         `json:"job_id"`
	Status  string         `json:"status" jsonschema:"The job's state: running, completed, failed, canceled, terminated, timed_out, or paused. Only completed has a report."`
	Report  *report.Report `json:"report,omitempty" jsonschema:"The report, once the job completed."`
	Failure *JobFailure    `json:"failure,omitempty" jsonschema:"Why the job failed, if it did."`
	Message string         `json:"message" jsonschema:"What the result means and what to do next."`
}

// JobFailure explains why a job failed.
type JobFailure struct {
	Type      string `json:"type" jsonschema:"The error type: one of the report error types, Timeout, or Unknown."`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable" jsonschema:"Whether starting a new report later could succeed. If false, the same request will fail again."`
}

func (t *tools) jobResult(ctx context.Context, _ *mcp.CallToolRequest, in JobInput) (*mcp.CallToolResult, JobResultOutput, error) {
	resp, err := t.describeJob(ctx, "job_result", in.JobID)
	if err != nil {
		return nil, JobResultOutput{}, err
	}
	out := JobResultOutput{JobID: in.JobID, Status: runStatus(resp.GetWorkflowExecutionInfo().GetStatus())}
	runID := resp.GetWorkflowExecutionInfo().GetExecution().GetRunId()

	switch out.Status {
	case statusRunning, statusPaused:
		out.Message = "The report is not ready yet: the job is still " + out.Status + ". " +
			"Call job_status to follow it, and job_result again once its status is completed."
		return nil, out, nil
	case statusCanceled:
		out.Message = "The job was canceled, so there is no report. Start a new report if you still need one."
		return nil, out, nil
	case statusTerminated:
		out.Message = "The job was terminated by an operator, so there is no report. Start a new report if you still need one."
		return nil, out, nil
	case statusTimedOut:
		out.Failure = &JobFailure{Type: "Timeout", Message: fmt.Sprintf("the job ran longer than its %s limit", JobTimeout), Retryable: true}
		out.Message = "The job timed out, so there is no report. Start a new report, perhaps with a shorter window, if you still need one."
		return nil, out, nil
	case statusCompleted, statusFailed:
	default:
		out.Message = "The job is in an unexpected state. Check job_status again later."
		return nil, out, nil
	}

	// The run is closed, so Get returns without waiting.
	getCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	var r report.Report
	err = t.client.GetWorkflow(getCtx, in.JobID, runID).
		GetWithOptions(getCtx, &r, client.WorkflowRunGetOptions{DisableFollowingRuns: true})
	if err == nil {
		out.Status = statusCompleted
		out.Report = &r
		out.Message = "The job completed; the report is in report."
		return nil, out, nil
	}
	if isCallError(getCtx, err) {
		// Not the job's failure: the call to Temporal failed.
		return nil, JobResultOutput{}, t.temporalError(getCtx, "job_result", in.JobID, err)
	}
	out.Status = statusFailed
	out.Failure = classify(err)
	out.Message = "The job failed: " + out.Failure.Message + ". " + nextStep(out.Failure.Type)
	return nil, out, nil
}

// isCallError reports whether err, from getting a closed run's result, is
// a failure of the call rather than the run's own error, which Get returns
// as a *temporal.WorkflowExecutionError.
func isCallError(ctx context.Context, err error) bool {
	_, isService := errors.AsType[serviceerror.ServiceError](err)
	return isService || ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// classify describes why a job failed, from the application error that
// caused it. Error values do not survive the trip through workflow history,
// so the application error's type is what identifies the cause.
func classify(err error) *JobFailure {
	if appErr, ok := errors.AsType[*temporal.ApplicationError](err); ok {
		errType := appErr.Type()
		if errType == "" {
			errType = "Unknown"
		}
		retryable := errType == report.ErrorTypeRateLimited || errType == report.ErrorTypeUnavailable
		return &JobFailure{Type: errType, Message: truncate(appErr.Message(), maxMessageLength), Retryable: retryable}
	}
	if timeoutErr, ok := errors.AsType[*temporal.TimeoutError](err); ok {
		return &JobFailure{Type: "Timeout", Message: truncate(timeoutErr.Message(), maxMessageLength), Retryable: true}
	}
	msg := err.Error()
	if wfErr, ok := errors.AsType[*temporal.WorkflowExecutionError](err); ok && errors.Unwrap(wfErr) != nil {
		msg = errors.Unwrap(wfErr).Error()
	}
	return &JobFailure{Type: "Unknown", Message: truncate(msg, maxMessageLength)}
}

// nextStep tells the agent what to do about a failure of the given type.
func nextStep(errType string) string {
	switch errType {
	case report.ErrorTypeRateLimited:
		return "GitHub's rate limit lasted longer than the job could wait. Start a new report later; " +
			"if this keeps happening, the worker needs a GitHub token with a higher limit."
	case report.ErrorTypeUnavailable, "Timeout":
		return "GitHub kept failing or did not respond. Start a new report in a few minutes."
	case report.ErrorTypeUnauthorized:
		return "GitHub rejected the worker's token. Do not retry: the server's operator must fix GITHUB_TOKEN. Tell the user."
	case report.ErrorTypeForbidden:
		return "The worker's GitHub token is not allowed to read this repository. Do not retry; tell the user."
	case report.ErrorTypeNotFound:
		return "The repository does not exist, or is private and the worker's token cannot see it. " +
			"Check owner and repo; do not retry the same request."
	case report.ErrorTypeInvalidRequest, report.ErrorTypeValidationFailed:
		return "The request was invalid. Fix the arguments before starting a new report."
	case report.ErrorTypeRequestRejected, report.ErrorTypeResponseTooLarge:
		return "GitHub's answer cannot be used for this repository. Do not retry; tell the user."
	default:
		return "Starting a new report may not help; if it fails again, stop and tell the user, with the job ID."
	}
}
