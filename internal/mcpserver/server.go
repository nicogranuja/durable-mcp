package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"google.golang.org/grpc/codes"

	"github.com/nicogranuja/durable-mcp/internal/auth"
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
	s.AddReceivingMiddleware(withCaller)
	t := &tools{client: c, taskQueue: opts.TaskQueue, logger: logger}
	t.register(s)
	return s
}

// withCaller puts the authenticated caller, if there is one, in the
// context of each request the server receives, so that handlers can read
// it with [auth.CallerFrom]. The HTTP transport's auth middleware passes
// the caller's name to the SDK as the token info's user ID; over stdio,
// and over HTTP without auth, there is no token info and no caller.
//
// The stateless HTTP handler happens to run handlers in the HTTP request's
// context, which auth.Middleware already gave the caller, but the SDK does
// not promise that (a stateful session's requests do not), while the token
// info is its documented way to pass the caller on.
func withCaller(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil && extra.TokenInfo.UserID != "" {
			ctx = auth.WithCaller(ctx, auth.Caller{Name: extra.TokenInfo.UserID})
		}
		return next(ctx, method, req)
	}
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

// describeJob describes the latest run of a job that the caller in ctx
// owns. A job that does not exist, is not a report, or belongs to another
// caller is reported as not found, with the same answer whichever it is,
// so that a caller cannot learn whether another caller's job exists. The
// type check keeps the tools to report jobs, so cancel_job cannot cancel
// other workflows in the namespace.
func (t *tools) describeJob(ctx context.Context, op, jobID string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	resp, err := t.lookupJob(ctx, op, jobID)
	switch {
	case err == nil:
		return resp, nil
	case errors.Is(err, errNotOwned), errors.Is(err, errNotReport):
		return nil, notFoundError(jobID)
	}
	if _, ok := errors.AsType[*serviceerror.NotFound](err); ok {
		return nil, notFoundError(jobID)
	}
	return nil, err
}

// Reasons lookupJob gives for not returning a job that Temporal has.
var (
	errNotReport = errors.New("the workflow is not a report")
	errNotOwned  = errors.New("the job belongs to another caller")
)

// lookupJob is describeJob with the reason a job is not returned: a
// *serviceerror.NotFound from Temporal, errNotReport, or errNotOwned. Any
// other Temporal error is already a tool error.
func (t *tools) lookupJob(ctx context.Context, op, jobID string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	if err := validateJobID(jobID); err != nil {
		return nil, err
	}
	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := t.client.DescribeWorkflowExecution(rpcCtx, jobID, "")
	if err != nil {
		if nf, ok := errors.AsType[*serviceerror.NotFound](err); ok {
			return nil, nf
		}
		return nil, t.temporalError(rpcCtx, op, jobID, err)
	}
	info := resp.GetWorkflowExecutionInfo()
	if info.GetType().GetName() != reportWorkflowType {
		t.logger.Warn("Job ID names a workflow that is not a report",
			"Op", op, "JobID", jobID, "WorkflowType", info.GetType().GetName())
		return nil, errNotReport
	}
	if owner, ok := jobOwner(info.GetMemo()); !ok || owner != callerName(ctx) {
		// Logged for the operator, who may see both names; the caller
		// only ever gets not found.
		t.logger.Info("Job belongs to another caller", "Op", op, "JobID", jobID,
			"Caller", callerName(ctx), "Owner", owner, "OwnerReadable", ok)
		return nil, errNotOwned
	}
	return resp, nil
}

// callerName returns the name of the authenticated caller in ctx, or ""
// if there is none: over stdio, and over HTTP without tokens.
func callerName(ctx context.Context) string {
	c, _ := auth.CallerFrom(ctx)
	return c.Name
}

// jobOwner returns the caller that started a job, from its memo. A job
// with no caller field was started without a caller, as every job was
// before callers existed, and belongs to the empty caller. A field that
// does not decode as a string belongs to no one, so ok is false.
//
// Only start_report writes the field, when it starts the job; the report
// workflow never upserts its memo. Anyone who could change a memo can
// already write to the Temporal namespace, and so cancel or read any job
// there directly: the memo is trusted as much as the namespace is.
func jobOwner(memo *commonpb.Memo) (owner string, ok bool) {
	payload := memo.GetFields()[memoCaller]
	if payload == nil {
		return "", true
	}
	if converter.GetDefaultDataConverter().FromPayload(payload, &owner) != nil {
		return "", false
	}
	return owner, true
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

	if isUnreachable(ctx, err) {
		return fmt.Errorf("%s: the job service (Temporal) did not respond in time or is unavailable. "+
			"Jobs are not affected. Retry this call in a few seconds; if it keeps failing, "+
			"tell the user that the durable-mcp server cannot reach Temporal", op)
	}
	return fmt.Errorf("%s: the job service (Temporal) returned an unexpected error (code %s). "+
		"Retry once; if it fails again, stop and tell the user, with the job ID if you have one",
		op, serviceerror.ToStatus(err).Code())
}

// isUnreachable reports whether err means that Temporal did not answer in
// time or could not be reached. Such a call may still have taken effect on
// the server.
func isUnreachable(ctx context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return true
	}
	switch serviceerror.ToStatus(err).Code() {
	case codes.DeadlineExceeded, codes.Unavailable, codes.ResourceExhausted, codes.Canceled:
		return true
	}
	return false
}

// jobStates are the values of a job's status.
var jobStates = []any{
	statusRunning, statusCompleted, statusFailed, statusCanceled, statusTerminated, statusTimedOut, statusPaused, statusUnknown,
}

// outputSchema returns the output schema that AddTool would infer for T,
// with the allowed values of some of its string properties listed as
// enums, so that an agent sees them in the schema rather than only in the
// descriptions.
func outputSchema[T any](enums map[string][]any) *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("output schema for %T: %v", *new(T), err))
	}
	for name, values := range enums {
		p, ok := s.Properties[name]
		if !ok {
			panic(fmt.Sprintf("output schema for %T has no property %q", *new(T), name))
		}
		p.Enum = values
	}
	return s
}
