package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"google.golang.org/grpc/codes"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// Client is the part of the Temporal client that the tools use. A
// [client.Client] satisfies it; tests use a fake.
type Client interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error)
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	QueryWorkflow(ctx context.Context, workflowID, runID, queryType string, args ...any) (converter.EncodedValue, error)
	GetWorkflow(ctx context.Context, workflowID, runID string) client.WorkflowRun
	CancelWorkflow(ctx context.Context, workflowID, runID string) error
}

var _ Client = client.Client(nil)

const (
	// JobTimeout is the workflow execution timeout of every report job: the
	// most a job can take, rate-limit waits included, before Temporal ends
	// it as timed out. The slowest legitimate report is one on a worker
	// without a GitHub token, which GitHub allows 60 requests an hour: the
	// largest report, 601 requests, takes about ten hours. A day leaves room
	// for that and still ends a stuck job within a day.
	JobTimeout = 24 * time.Hour

	// rpcTimeout bounds each Temporal call a tool makes, so a tool call
	// fails with an answer the agent can act on instead of hanging.
	rpcTimeout = 10 * time.Second

	// queryTimeout bounds the progress query. A query needs a worker to
	// answer it, so without one it would wait for the whole rpcTimeout;
	// job_status then answers without progress instead.
	queryTimeout = 5 * time.Second

	// reportWorkflowType is the name the worker registers
	// report.RepositoryReport under. The Go SDK takes it from the function
	// name.
	reportWorkflowType = "RepositoryReport"
)

// Options configures the MCP server.
type Options struct {
	// TaskQueue is the task queue the worker polls. Empty means
	// report.DefaultTaskQueue.
	TaskQueue string

	// Version is the server version reported to clients.
	Version string

	// Logger receives the server's logs, including the details of Temporal
	// errors that tool results leave out. Nil discards them.
	Logger *slog.Logger
}

// instructions tell the agent how the tools fit together.
const instructions = `durable-mcp runs GitHub repository reports as durable background jobs.
start_report returns a job_id at once; the report itself can take from seconds to hours,
for example while it waits out a GitHub rate limit. Poll job_status every so often
(it says what the job is doing and when it will resume after a wait), then call
job_result once the status is completed. cancel_job stops a job you no longer need.
Jobs keep running if this conversation or server restarts; keep the job_id to come back to them.`

// New returns an MCP server with the report tools, which run jobs through
// c.
func New(c Client, opts Options) *mcp.Server {
	if opts.TaskQueue == "" {
		opts.TaskQueue = report.DefaultTaskQueue
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "durable-mcp", Version: opts.Version}, &mcp.ServerOptions{
		Instructions: instructions,
		Logger:       logger,
		// Not the SDK's default logging capability, which protocol version
		// 2026-07-28 deprecates: the server's logs go to its operator.
		Capabilities: &mcp.ServerCapabilities{},
	})
	t := &tools{client: c, taskQueue: opts.TaskQueue, logger: logger}
	t.register(s)
	return s
}

// tools implements the MCP tools.
type tools struct {
	client    Client
	taskQueue string
	logger    *slog.Logger
}

func (t *tools) register(s *mcp.Server) {
	mcp.AddTool(s, startReportTool, t.startReport)
	mcp.AddTool(s, jobStatusTool, t.jobStatus)
	mcp.AddTool(s, jobResultTool, t.jobResult)
	mcp.AddTool(s, cancelJobTool, t.cancelJob)
}

// describeJob describes the latest run of the job, and checks that the job
// is a report. The check keeps the tools to report jobs: other workflows in
// the namespace are reported as not found, so cancel_job cannot cancel them.
func (t *tools) describeJob(ctx context.Context, op, jobID string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	if err := validateJobID(jobID); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := t.client.DescribeWorkflowExecution(ctx, jobID, "")
	if err != nil {
		return nil, t.temporalError(ctx, op, jobID, err)
	}
	if resp.GetWorkflowExecutionInfo().GetType().GetName() != reportWorkflowType {
		t.logger.Warn("Job ID names a workflow that is not a report",
			"Op", op, "JobID", jobID, "WorkflowType", resp.GetWorkflowExecutionInfo().GetType().GetName())
		return nil, notFoundError(jobID)
	}
	return resp, nil
}

// notFoundError is the tool error for a job that does not exist.
func notFoundError(jobID string) error {
	return fmt.Errorf("no job with ID %q exists. Check that it is a job_id that start_report returned; "+
		"finished jobs are also deleted once the Temporal namespace's retention period passes. "+
		"Retrying will not help; start a new report if you still need one", jobID)
}

// temporalError turns an error from a Temporal call into a tool error. The
// agent gets what happened and what to do next; the operator gets the
// details in the log. Server error messages can name internal hosts and
// state, so they stay out of tool results.
func (t *tools) temporalError(ctx context.Context, op, jobID string, err error) error {
	if _, ok := errors.AsType[*serviceerror.NotFound](err); ok {
		return notFoundError(jobID)
	}
	t.logger.Warn("Temporal call failed", "Op", op, "JobID", jobID, "Error", err)

	code := serviceerror.ToStatus(err).Code()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil,
		code == codes.DeadlineExceeded, code == codes.Unavailable, code == codes.ResourceExhausted,
		code == codes.Canceled:
		return fmt.Errorf("%s: the job service (Temporal) did not respond in time or is unavailable. "+
			"Jobs are not affected. Retry this call in a few seconds; if it keeps failing, "+
			"tell the user that the durable-mcp server cannot reach Temporal", op)
	default:
		return fmt.Errorf("%s: the job service (Temporal) returned an unexpected error (code %s). "+
			"Retry once; if it fails again, stop and tell the user, with the job ID if you have one", op, code)
	}
}
