package mcpserver

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.temporal.io/api/serviceerror"
)

var cancelJobTool = &mcp.Tool{
	Name:  "cancel_job",
	Title: "Cancel a report job",
	Description: `Cancel a job that start_report started, if you no longer need its report.

Cancellation is requested, not immediate: the job stops its current GitHub request and ends as canceled, usually within seconds
(a job paused by an operator stops once it is unpaused).
Call job_status to confirm. Calling cancel_job again, or on a job that already finished, is safe and changes nothing.
A canceled job cannot be resumed; start a new report instead.`,
	Annotations: &mcp.ToolAnnotations{
		Title:           "Cancel a report job",
		DestructiveHint: new(true),
		IdempotentHint:  true,
		OpenWorldHint:   new(false),
	},
}

// CancelJobOutput is the result of cancel_job.
type CancelJobOutput struct {
	JobID           string `json:"job_id"`
	Status          string `json:"status" jsonschema:"The job's state after this call: running while it stops, or the state it had already finished in."`
	CancelRequested bool   `json:"cancel_requested" jsonschema:"True if the job is stopping because of a cancellation request, from this call or an earlier one."`
	Message         string `json:"message"`
}

func (t *tools) cancelJob(ctx context.Context, _ *mcp.CallToolRequest, in JobInput) (*mcp.CallToolResult, CancelJobOutput, error) {
	resp, err := t.describeJob(ctx, "cancel_job", in.JobID)
	if err != nil {
		return nil, CancelJobOutput{}, err
	}
	st := runStatus(resp.GetWorkflowExecutionInfo().GetStatus())
	if st != statusRunning && st != statusPaused {
		return nil, finishedOutput(in.JobID, st), nil
	}
	if resp.GetWorkflowExtendedInfo().GetCancelRequested() {
		return nil, stoppingOutput(in.JobID, st, "Cancellation was already requested; the job is stopping."), nil
	}

	// Cancel the run that was described, so that a cancellation can never
	// reach a later run with the same ID.
	runID := resp.GetWorkflowExecutionInfo().GetExecution().GetRunId()
	cancelCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	err = t.client.CancelWorkflow(cancelCtx, in.JobID, runID)
	switch {
	case err == nil:
		t.logger.Info("Requested job cancellation", "JobID", in.JobID)
		return nil, stoppingOutput(in.JobID, st, "Cancellation requested; the job is stopping."), nil
	case isAlreadyRequested(err):
		return nil, stoppingOutput(in.JobID, st, "Cancellation was already requested; the job is stopping."), nil
	}
	if _, ok := errors.AsType[*serviceerror.NotFound](err); ok {
		// The run most likely finished between the describe and the
		// cancel. The job exists, so this is never "no such job".
		resp, derr := t.describeJob(ctx, "cancel_job", in.JobID)
		if derr == nil {
			if st := runStatus(resp.GetWorkflowExecutionInfo().GetStatus()); st != statusRunning && st != statusPaused {
				return nil, finishedOutput(in.JobID, st), nil
			}
		}
		t.logger.Warn("Cancel found no open run of a job that exists", "JobID", in.JobID, "Error", err)
		return nil, CancelJobOutput{}, errors.New("cancel_job: the job changed while it was being canceled. " +
			"Call cancel_job again; that is safe")
	}
	return nil, CancelJobOutput{}, t.temporalError(cancelCtx, "cancel_job", in.JobID, err)
}

func isAlreadyRequested(err error) bool {
	_, ok := errors.AsType[*serviceerror.CancellationAlreadyRequested](err)
	return ok
}

func stoppingOutput(jobID, st, what string) CancelJobOutput {
	msg := what + " Call job_status to confirm that it ends as canceled; there is no report."
	if st == statusPaused {
		msg = what + " The job is paused by an operator, so it stops only once it is unpaused; there is no report."
	}
	return CancelJobOutput{JobID: jobID, Status: st, CancelRequested: true, Message: msg}
}

func finishedOutput(jobID, st string) CancelJobOutput {
	return CancelJobOutput{
		JobID:   jobID,
		Status:  st,
		Message: "The job already finished as " + st + ", so there was nothing to cancel.",
	}
}
