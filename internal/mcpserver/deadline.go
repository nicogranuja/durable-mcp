package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultToolTimeout is the overall deadline of a tool call when
// [Options.ToolTimeout] is zero. Each Temporal call a tool makes has its
// own, shorter timeout (rpcTimeout, queryTimeout); this one bounds the
// call as a whole. The slowest tool, cancel_job, makes at most three
// Temporal calls of up to rpcTimeout each, so 30 seconds lets every tool
// finish normally, and still answers well within the HTTP transport's
// one-minute write timeout.
const DefaultToolTimeout = 30 * time.Second

// methodCallTool is the MCP method of a tool call.
const methodCallTool = "tools/call"

// withToolDeadline returns receiving middleware that gives every tool
// call an overall deadline of d, so that a call answers in time even when
// its Temporal calls, one after another, would add up to more.
//
// The tools pass the call's context to every Temporal call, so once the
// deadline passes, the call in progress fails and the tool returns. A tool
// already turns that failure into its own tool error, which says what is
// safe to do next (start_report, for one, says whether the job may have
// started), so that answer is kept. A tool that finished just as the
// deadline passed keeps its result too. Only a call that failed with a
// protocol error once the deadline passed gets the generic tool error
// below instead, so the agent always learns that the call timed out.
func withToolDeadline(d time.Duration, logger *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != methodCallTool {
				return next(ctx, method, req)
			}
			callCtx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			res, err := next(callCtx, method, req)
			// Only this deadline: a call canceled by the client, or by the
			// HTTP request ending, is not a timeout.
			if ctx.Err() != nil || !errors.Is(callCtx.Err(), context.DeadlineExceeded) {
				return res, err
			}
			tool := toolName(req)
			logger.Warn("Tool call ran past its deadline", "Tool", tool, "Deadline", d, "Error", err)
			if err == nil && res != nil {
				return res, nil
			}
			out := &mcp.CallToolResult{}
			out.SetError(deadlineError(tool, d))
			return out, nil
		}
	}
}

// toolName returns the name of the tool that req calls.
func toolName(req mcp.Request) string {
	if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
		return p.Name
	}
	return ""
}

// deadlineError is the tool error for a call that ran past its deadline.
// The call may have taken effect, so the advice is what is safe to repeat.
func deadlineError(tool string, d time.Duration) error {
	next := "Retry the call in a few seconds"
	switch tool {
	case "start_report":
		next = "The job may or may not have started. With an idempotency_key, retrying in a few seconds with " +
			"the same arguments and key is safe and returns the job if it did start; without one, a retry may " +
			"start a second job"
	case "cancel_job":
		next = "Cancellation may or may not have been requested. Call cancel_job again in a few seconds; that is safe"
	}
	return fmt.Errorf("%s: the call did not finish within its %s limit, most likely because the job service "+
		"(Temporal) is slow or unreachable. %s; if it keeps failing, tell the user that the durable-mcp server "+
		"cannot reach Temporal", tool, d, next)
}
