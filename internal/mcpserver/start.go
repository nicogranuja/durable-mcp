package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// memoFingerprint is the memo field that records which request a job was
// started for.
const memoFingerprint = "request_fingerprint"

var startReportTool = &mcp.Tool{
	Name:  "start_report",
	Title: "Start a GitHub repository report",
	Description: `Start a background job that reports on a public or token-accessible GitHub repository's activity over a time window:
issues opened and closed (and how), pull requests opened, merged, and closed, median time to merge, commits per week, and top contributors.

Returns a job_id immediately; the report itself runs durably on the server and can take from seconds to hours
(large repositories have many pages, and GitHub rate limits can force waits). Follow it with job_status and fetch it with job_result.

The window defaults to the 30 days before the job starts and can be at most 366 days long; it cannot start in the future.

Always pass an idempotency_key, a fresh UUID you generate for each new report, and reuse it if you retry this call:
a retry with the same key and arguments returns the same job instead of starting a second one, even after the job finished,
and with the window it was started with. Keys are shared by everyone using this server, so do not use short or guessable keys.
Reusing a key with different arguments is an error.`,
	OutputSchema: outputSchema[StartReportOutput](map[string][]any{"status": jobStates}),
	Annotations: &mcp.ToolAnnotations{
		DestructiveHint: new(false),
		OpenWorldHint:   new(true),
	},
}

// StartReportInput is the input of start_report.
type StartReportInput struct {
	Owner          string `json:"owner" jsonschema:"Repository owner: the user or organization in github.com/OWNER/REPO, such as temporalio. Up to 39 letters, digits, and hyphens."`
	Repo           string `json:"repo" jsonschema:"Repository name: REPO in github.com/OWNER/REPO, such as sdk-go, without .git. Up to 100 letters, digits, '-', '_', and '.'."`
	Since          string `json:"since,omitempty" jsonschema:"Start of the window, inclusive. An RFC 3339 time such as 2026-10-01T00:00:00Z, or a date such as 2026-10-01 (midnight UTC). Defaults to 30 days before until."`
	Until          string `json:"until,omitempty" jsonschema:"End of the window, exclusive, in the same formats as since. Defaults to the time the job starts."`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Key that makes retries safe: calls with the same key and arguments return the same job. 8 to 128 ASCII letters, digits, '-', '_', '.', or ':'. Use a fresh UUID for each new report and the same one when retrying."`
}

// StartReportOutput is the result of start_report.
type StartReportOutput struct {
	JobID   string `json:"job_id" jsonschema:"The job's ID. Pass it to job_status, job_result, and cancel_job."`
	Status  string `json:"status" jsonschema:"The job's state: running for a new job; for an existing job returned for an idempotency key, its current state."`
	Created bool   `json:"created" jsonschema:"True if this call started the job; false if the idempotency key matched a job started earlier."`
	Message string `json:"message" jsonschema:"What happened and what to do next."`
}

// validate checks the tool input as of now and converts it to a workflow
// request. The error lists every problem, so an agent can fix them all in
// one go.
func (in StartReportInput) validate(now time.Time) (report.Request, error) {
	var problems []string
	since, sinceErr := parseTime("since", in.Since)
	until, untilErr := parseTime("until", in.Until)
	for _, err := range []error{sinceErr, untilErr} {
		if err != nil {
			problems = append(problems, err.Error())
		}
	}
	req := report.Request{Owner: in.Owner, Repo: in.Repo, Since: since, Until: until}
	if err := req.Validate(now); err != nil {
		problems = append(problems, err.Error())
	}
	if in.IdempotencyKey != "" {
		if err := validateKey(in.IdempotencyKey); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return report.Request{}, errors.New(strings.Join(problems, "; "))
	}
	return req, nil
}

// parseTime parses an RFC 3339 time or a date, as UTC midnight. An empty
// string is the zero time, which the workflow replaces with its default.
func parseTime(name, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%s %q is not a date or time: use RFC 3339, such as 2026-10-01T00:00:00Z, "+
		"or a date, such as 2026-10-01", name, truncate(s, 40))
}

// inputError is the tool error for invalid arguments.
func inputError(err error) error {
	return fmt.Errorf("invalid arguments: %w. Fix the arguments and call start_report again; no job was started", err)
}

// startOptions returns the options every job starts with.
//
//   - The workflow ID is the job ID.
//   - JobTimeout is the workflow execution timeout; see its documentation.
//   - The ID reuse policy, RejectDuplicate, means a job ID names one run,
//     ever: a retried start with the same idempotency key finds the job it
//     started before, even after that job finished, rather than running
//     the report again. This holds until the namespace's retention period
//     deletes the finished job.
//   - The ID conflict policy, Fail, means a start never attaches to or
//     replaces a running job. With WorkflowExecutionErrorWhenAlreadyStarted
//     the duplicate comes back as an error, so the tool can tell a new job
//     from an existing one and check that it is for the same request.
func (t *tools) startOptions(jobID, fp string, req report.Request) client.StartWorkflowOptions {
	return client.StartWorkflowOptions{
		ID:                                       jobID,
		TaskQueue:                                t.taskQueue,
		WorkflowExecutionTimeout:                 JobTimeout,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowIDConflictPolicy:                 enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
		Memo:                                     map[string]any{memoFingerprint: fp},
		StaticSummary:                            "Report on " + req.Owner + "/" + req.Repo,
	}
}

func (t *tools) startReport(ctx context.Context, _ *mcp.CallToolRequest, in StartReportInput) (*mcp.CallToolResult, StartReportOutput, error) {
	req, err := in.validate(time.Now())
	if err != nil {
		return nil, StartReportOutput{}, inputError(err)
	}
	jobID := newJobID()
	if in.IdempotencyKey != "" {
		jobID = jobIDForKey(in.IdempotencyKey)
	}
	fp := fingerprint(req)

	startCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	_, err = t.client.ExecuteWorkflow(startCtx, t.startOptions(jobID, fp, req), report.RepositoryReport, req)
	if _, ok := errors.AsType[*serviceerror.WorkflowExecutionAlreadyStarted](err); ok && in.IdempotencyKey != "" {
		return t.existingJob(ctx, jobID, fp)
	}
	if err != nil {
		return nil, StartReportOutput{}, t.startError(startCtx, jobID, in.IdempotencyKey != "", err)
	}
	t.logger.Info("Started report job", "JobID", jobID, "Owner", req.Owner, "Repo", req.Repo)
	return nil, StartReportOutput{
		JobID:   jobID,
		Status:  statusRunning,
		Created: true,
		Message: "The report job started. Call job_status with this job_id to follow it, and job_result once its status is completed.",
	}, nil
}

// startError turns an error from starting a job into a tool error. When
// Temporal did not answer in time, the start may still have succeeded, so
// the advice must not lead to a second job: with a key, the same call is
// safe to repeat; without one, the agent gets the job ID that may exist.
func (t *tools) startError(ctx context.Context, jobID string, haveKey bool, err error) error {
	if !isUnreachable(ctx, err) {
		return t.temporalError(ctx, "start_report", jobID, err)
	}
	t.logger.Warn("Temporal call failed", "Op", "start_report", "JobID", jobID, "Error", err)
	if haveKey {
		return errors.New("start_report: the job service (Temporal) did not respond in time or is unavailable, " +
			"so the job may or may not have started. Retry this call in a few seconds with the same arguments and " +
			"idempotency_key: that is safe, and returns the job if it did start")
	}
	return fmt.Errorf("start_report: the job service (Temporal) did not respond in time or is unavailable, "+
		"so the job may or may not have started; if it did, its job_id is %q. In a few seconds, call job_status "+
		"with that job_id before starting another report (it says no job exists if none started), and pass an "+
		"idempotency_key so that retries are safe", jobID)
}

// existingJob answers a start whose idempotency key names a job that
// already exists, after checking that the job is for the same request.
func (t *tools) existingJob(ctx context.Context, jobID, fp string) (*mcp.CallToolResult, StartReportOutput, error) {
	resp, err := t.describeJob(ctx, "start_report", jobID)
	if err != nil {
		return nil, StartReportOutput{}, err
	}
	// A job without a readable fingerprint was not started by start_report,
	// so it cannot be shown to be for this request.
	var got string
	payload := resp.GetWorkflowExecutionInfo().GetMemo().GetFields()[memoFingerprint]
	if payload == nil || converter.GetDefaultDataConverter().FromPayload(payload, &got) != nil || got != fp {
		if got == "" {
			t.logger.Warn("Job for an idempotency key has no request fingerprint", "JobID", jobID)
		}
		return nil, StartReportOutput{}, errors.New("invalid arguments: this idempotency_key was already used " +
			"to start a report with different arguments. Use a new idempotency_key for a different report, " +
			"or repeat the original arguments to get the original job")
	}
	st := runStatus(resp.GetWorkflowExecutionInfo().GetStatus())
	return nil, StartReportOutput{
		JobID:   jobID,
		Status:  st,
		Created: false,
		Message: fmt.Sprintf("A job for this idempotency_key already exists (status %s), so no new job was started. "+
			"Use this job_id with job_status and job_result.", st),
	}, nil
}
