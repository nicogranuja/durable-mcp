package mcpserver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	enumspb "go.temporal.io/api/enums/v1"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// TestEveryTemporalCallHasDeadline calls every tool, along paths that
// between them make every Client call, and checks that each call got a
// context with a deadline no later than its own timeout allows.
func TestEveryTemporalCallHasDeadline(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	cs := connect(t, f)
	start := map[string]any{"owner": "acme", "repo": "widgets", "idempotency_key": "deadlines-1"}
	started := structured[StartReportOutput](t, call(t, cs, "start_report", start))
	structured[StartReportOutput](t, call(t, cs, "start_report", start)) // the existing job
	args := map[string]any{"job_id": started.JobID}
	structured[JobStatusOutput](t, call(t, cs, "job_status", args))
	structured[CancelJobOutput](t, call(t, cs, "cancel_job", args))

	f.mu.Lock()
	j := f.jobs[started.JobID]
	j.status = enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED
	j.closeTime = time.Now()
	j.result = report.Report{Repository: report.Repository{FullName: "acme/widgets"}}
	f.mu.Unlock()
	structured[JobResultOutput](t, call(t, cs, "job_result", args))

	calls := f.recorded()
	var methods []string
	for _, c := range calls {
		if !slices.Contains(methods, c.method) {
			methods = append(methods, c.method)
		}
		switch {
		case c.deadline.IsZero():
			t.Errorf("%s was called without a deadline", c.method)
		case c.deadline.Sub(c.at) > rpcTimeout:
			t.Errorf("%s was called with %s left, want at most rpcTimeout (%s)", c.method, c.deadline.Sub(c.at), rpcTimeout)
		}
	}
	want := []string{"ExecuteWorkflow", "DescribeWorkflowExecution", "QueryWorkflow", "CancelWorkflow", "GetWorkflow", "WorkflowRun.Get"}
	for _, m := range want {
		if !slices.Contains(methods, m) {
			t.Errorf("no call to %s; the test no longer covers it (called %v)", m, methods)
		}
	}
}

// TestToolDeadline checks that when Temporal does not answer, every tool
// returns at its overall deadline, not after its Temporal calls' longer
// timeouts, with a tool error that says what to do next.
func TestToolDeadline(t *testing.T) {
	t.Parallel()

	const deadline = 200 * time.Millisecond
	f := newFakeClient()
	f.add(testJobID, enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING)
	f.hang.Store(true)
	cs := connectWith(t, f, Options{ToolTimeout: deadline})

	job := map[string]any{"job_id": testJobID}
	for _, tt := range []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{name: "start with a key", tool: "start_report", args: map[string]any{"owner": "acme", "repo": "widgets", "idempotency_key": "deadline-key-1"},
			want: "Retry this call in a few seconds with the same arguments and idempotency_key"},
		{name: "start without a key", tool: "start_report", args: map[string]any{"owner": "acme", "repo": "widgets"},
			want: "call job_status with that job_id before starting another report"},
		{name: "status", tool: "job_status", args: job, want: "did not respond in time"},
		{name: "result", tool: "job_result", args: job, want: "did not respond in time"},
		{name: "cancel", tool: "cancel_job", args: job, want: "did not respond in time"},
	} {
		begin := time.Now()
		res := call(t, cs, tt.tool, tt.args)
		elapsed := time.Since(begin)
		if !res.IsError || !strings.Contains(text(res), tt.want) {
			t.Errorf("%s: result = error %v, %q; want a tool error containing %q", tt.name, res.IsError, text(res), tt.want)
		}
		// Well below rpcTimeout, which the call would wait for without
		// the overall deadline, and generous for a slow CI machine.
		if elapsed > rpcTimeout/2 {
			t.Errorf("%s: took %s, want about the %s deadline", tt.name, elapsed, deadline)
		}
	}
}

func TestWithToolDeadline(t *testing.T) {
	t.Parallel()

	const d = 50 * time.Millisecond
	toolCall := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "cancel_job"}}
	succeeded := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}
	errProtocol := errors.New("protocol error")

	// waitThen returns a handler that waits until its context is done,
	// then answers res and err.
	waitThen := func(res mcp.Result, err error) mcp.MethodHandler {
		return func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
			<-ctx.Done()
			return res, err
		}
	}

	t.Run("protocol error past the deadline", func(t *testing.T) {
		t.Parallel()
		h := withToolDeadline(d, discardLogger())(waitThen(nil, errProtocol))
		res, err := h(t.Context(), methodCallTool, toolCall)
		r, ok := res.(*mcp.CallToolResult)
		if err != nil || !ok || !r.IsError {
			t.Fatalf("got %#v, %v; want a tool error", res, err)
		}
		if got := text(r); !strings.Contains(got, "within its 50ms limit") || !strings.Contains(got, "Call cancel_job again") {
			t.Errorf("tool error = %q, want the deadline and cancel_job's advice", got)
		}
	})

	t.Run("result past the deadline is kept", func(t *testing.T) {
		t.Parallel()
		h := withToolDeadline(d, discardLogger())(waitThen(succeeded, nil))
		if res, err := h(t.Context(), methodCallTool, toolCall); res != succeeded || err != nil {
			t.Errorf("got %#v, %v; want the tool's own result", res, err)
		}
	})

	t.Run("canceled by the client", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		h := withToolDeadline(d, discardLogger())(waitThen(nil, errProtocol))
		if res, err := h(ctx, methodCallTool, toolCall); res != nil || !errors.Is(err, errProtocol) {
			t.Errorf("got %#v, %v; want the handler's error, not a timeout", res, err)
		}
	})

	t.Run("other methods have no deadline", func(t *testing.T) {
		t.Parallel()
		h := withToolDeadline(d, discardLogger())(func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
			if _, ok := ctx.Deadline(); ok {
				t.Error("tools/list got a deadline")
			}
			return nil, nil
		})
		_, _ = h(t.Context(), "tools/list", &mcp.ListToolsRequest{})
	})
}
