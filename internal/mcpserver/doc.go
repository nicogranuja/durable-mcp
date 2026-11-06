// Package mcpserver is the MCP server: tools that let an AI agent run
// repository reports as durable Temporal jobs.
//
// [New] returns an [mcp.Server] that runs jobs through a [Client], the part
// of the Temporal client the tools use. The server has no state of its own:
// everything about a job lives in Temporal, so the server can restart, or
// run as several replicas, without losing a job.
//
// # Tools
//
// A report can take hours, which is far longer than an agent should hold
// a tool call open, so the work is split into four tools that each return
// within seconds:
//
//   - start_report validates the request, starts the RepositoryReport
//     workflow, and returns its job ID.
//   - job_status reports the job's state, from DescribeWorkflowExecution,
//     and its progress, from the workflow's progress query. A retry in
//     progress is not workflow state: Temporal keeps it in the pending
//     activity, with the attempt, the last failure, and the next attempt
//     time. That is where a page waiting for a GitHub rate limit shows up,
//     and job_status reports it as a wait until that time.
//   - job_result returns the report of a completed job, or says why there
//     is none: still running, failed (classified by the application
//     error's type, with whether a new report could succeed), canceled,
//     terminated, or timed out. It never waits for the job.
//   - cancel_job requests cancellation. Repeating it, or canceling a
//     finished job, changes nothing.
//
// Tool errors (IsError results) are for calls that could not be answered:
// invalid arguments, an unknown job ID, or Temporal being unreachable.
// Each says what happened and what to do next. A job that failed is not a
// tool error; job_result describes it. Details of Temporal errors are
// logged for the operator and left out of tool results.
//
// # Job IDs and idempotency
//
// A job ID is the workflow ID: "report-" and 26 base32 characters. Without
// an idempotency key the ID is random. With one, the ID is a hash of the
// key, so a retried call with the same key names the same workflow, and
// Temporal's ID reuse policy, RejectDuplicate, refuses to start it twice,
// even after the first run finished. The workflow's memo records a
// fingerprint of the request, so reusing a key for a different request is
// reported as an error instead of returning the wrong job.
//
// A job runs for at most [JobTimeout]. Finished jobs, and so the keys that
// map to them, are kept for the Temporal namespace's retention period.
//
// The tools accept only IDs of that shape, and only for RepositoryReport
// workflows, so an agent cannot read or cancel other workflows in the
// namespace through them.
package mcpserver
