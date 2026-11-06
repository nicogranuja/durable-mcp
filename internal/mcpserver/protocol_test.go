package mcpserver

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// connect serves New(f) over an in-memory transport and returns a client
// session connected to it.
func connect(t *testing.T, f *fakeClient) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := New(f, Options{TaskQueue: "test-queue", Version: "v0.0.0-test"}).Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.0"}, nil).
		Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call calls a tool and returns its result, failing the test on a
// protocol error.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

// structured decodes a successful tool result's structured content into
// a T, and checks that the text content carries the same JSON for clients
// that only read text.
func structured[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", text(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out, fromText T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode structured content %s: %v", b, err)
	}
	if err := json.Unmarshal([]byte(text(res)), &fromText); err != nil {
		t.Errorf("text content is not the JSON result: %v", err)
	}
	return out
}

// text is a result's text content.
func text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestServerInfo(t *testing.T) {
	t.Parallel()

	init := connect(t, newFakeClient()).InitializeResult()
	if init.ServerInfo.Name != "durable-mcp" || init.ServerInfo.Version != "v0.0.0-test" {
		t.Errorf("server info = %+v, want durable-mcp v0.0.0-test", init.ServerInfo)
	}
	if !strings.Contains(init.Instructions, "start_report returns a job_id") {
		t.Errorf("instructions = %q, want them to explain the tools", init.Instructions)
	}
	if init.Capabilities.Tools == nil {
		t.Errorf("capabilities = %+v, want tools", init.Capabilities)
	}
}

func TestListTools(t *testing.T) {
	t.Parallel()

	res, err := connect(t, newFakeClient()).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tools := make(map[string]*mcp.Tool)
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	if got, want := slices.Sorted(maps.Keys(tools)), []string{"cancel_job", "job_result", "job_status", "start_report"}; !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}

	tests := []struct {
		name       string
		properties []string
		required   []string
		output     []string // required output properties
		readOnly   bool
	}{
		{
			name:       "start_report",
			properties: []string{"idempotency_key", "owner", "repo", "since", "until"},
			required:   []string{"owner", "repo"},
			output:     []string{"created", "job_id", "message", "status"},
		},
		{name: "job_status", properties: []string{"job_id"}, required: []string{"job_id"}, output: []string{"job_id", "message", "status"}, readOnly: true},
		{name: "job_result", properties: []string{"job_id"}, required: []string{"job_id"}, output: []string{"job_id", "message", "status"}, readOnly: true},
		{name: "cancel_job", properties: []string{"job_id"}, required: []string{"job_id"}, output: []string{"cancel_requested", "job_id", "message", "status"}},
	}
	for _, tt := range tests {
		tool := tools[tt.name]
		if tool.Title == "" || len(tool.Description) < 100 {
			t.Errorf("%s: title %q, description %q: want both, and a description an agent can act on", tt.name, tool.Title, tool.Description)
		}
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != tt.readOnly {
			t.Errorf("%s: annotations = %+v, want read-only %v", tt.name, tool.Annotations, tt.readOnly)
		}

		in := schema(t, tool.InputSchema)
		if in.Type != "object" || in.AdditionalProperties == nil {
			t.Errorf("%s: input schema type %q, additionalProperties %v; want a closed object", tt.name, in.Type, in.AdditionalProperties)
		}
		if got := slices.Sorted(maps.Keys(in.Properties)); !slices.Equal(got, tt.properties) {
			t.Errorf("%s: input properties = %v, want %v", tt.name, got, tt.properties)
		}
		if got := slices.Sorted(slices.Values(in.Required)); !slices.Equal(got, tt.required) {
			t.Errorf("%s: required = %v, want %v", tt.name, got, tt.required)
		}
		for name, p := range in.Properties {
			if p.Type != "string" || p.Description == "" {
				t.Errorf("%s: property %s = %+v, want a described string", tt.name, name, p)
			}
		}

		out := schema(t, tool.OutputSchema)
		for _, p := range tt.output {
			if !slices.Contains(out.Required, p) {
				t.Errorf("%s: output schema requires %v, want it to require %q", tt.name, out.Required, p)
			}
		}
	}
	if !tools["cancel_job"].Annotations.IdempotentHint || !*tools["cancel_job"].Annotations.DestructiveHint {
		t.Errorf("cancel_job annotations = %+v, want destructive and idempotent", tools["cancel_job"].Annotations)
	}
	if tools["start_report"].Annotations.IdempotentHint {
		t.Error("start_report is marked idempotent, but without a key each call starts a job")
	}
}

// jsonSchema is the part of a JSON schema the tests check.
type jsonSchema struct {
	Type                 any                    `json:"type"` // a string, or an array for nullable types
	Description          string                 `json:"description"`
	Properties           map[string]*jsonSchema `json:"properties"`
	Required             []string               `json:"required"`
	AdditionalProperties any                    `json:"additionalProperties"`
}

func schema(t *testing.T, s any) jsonSchema {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var js jsonSchema
	if err := json.Unmarshal(b, &js); err != nil {
		t.Fatalf("decode schema %s: %v", b, err)
	}
	return js
}

// TestJobLifecycle drives a job through every tool, as an agent would.
func TestJobLifecycle(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	cs := connect(t, f)

	started := structured[StartReportOutput](t, call(t, cs, "start_report", map[string]any{
		"owner": "acme", "repo": "widgets", "since": "2026-10-01", "idempotency_key": "lifecycle-1",
	}))
	if !started.Created || started.Status != statusRunning || started.JobID != jobIDForKey("lifecycle-1") {
		t.Fatalf("start_report = %+v, want a new running job", started)
	}
	again := structured[StartReportOutput](t, call(t, cs, "start_report", map[string]any{
		"owner": "acme", "repo": "widgets", "since": "2026-10-01", "idempotency_key": "lifecycle-1",
	}))
	if again.Created || again.JobID != started.JobID {
		t.Errorf("retried start_report = %+v, want the same job, not created", again)
	}
	args := map[string]any{"job_id": started.JobID}

	// The job waits for a rate limit on its second issues page.
	resetAt := time.Now().Add(40 * time.Minute).UTC().Truncate(time.Second)
	f.mu.Lock()
	j := f.jobs[started.JobID]
	j.progress = &report.Progress{Phase: report.PhaseIssues, Pages: report.PageCounts{Issues: 1}}
	j.pending = []*workflowpb.PendingActivityInfo{pendingPage(report.ErrorTypeRateLimited, 2, resetAt)}
	f.mu.Unlock()

	status := structured[JobStatusOutput](t, call(t, cs, "job_status", args))
	if status.Status != statusRunning || status.CurrentStep == nil || !status.CurrentStep.NextAttemptAt.Equal(resetAt) {
		t.Errorf("job_status = %s, want a running job waiting until %v", mustJSON(status), resetAt)
	}
	if want := "Waiting for GitHub rate limit until " + resetAt.Format(time.RFC3339); !strings.Contains(status.Message, want) {
		t.Errorf("job_status message = %q, want it to contain %q", status.Message, want)
	}

	notReady := structured[JobResultOutput](t, call(t, cs, "job_result", args))
	if notReady.Status != statusRunning || notReady.Report != nil {
		t.Errorf("job_result = %s, want not ready", mustJSON(notReady))
	}

	// The job completes. An empty report, with nil slices, as for a
	// repository with no activity, must still satisfy the output schema.
	f.mu.Lock()
	j.status = enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED
	j.closeTime = time.Now()
	j.pending = nil
	j.result = report.Report{Repository: report.Repository{FullName: "acme/widgets"}}
	f.mu.Unlock()

	status = structured[JobStatusOutput](t, call(t, cs, "job_status", args))
	if status.Status != statusCompleted || status.ClosedAt.IsZero() {
		t.Errorf("job_status = %s, want completed", mustJSON(status))
	}
	result := structured[JobResultOutput](t, call(t, cs, "job_result", args))
	if result.Status != statusCompleted || result.Report == nil || result.Report.Repository.FullName != "acme/widgets" {
		t.Errorf("job_result = %s, want the report", mustJSON(result))
	}

	canceled := structured[CancelJobOutput](t, call(t, cs, "cancel_job", args))
	if canceled.CancelRequested || canceled.Status != statusCompleted {
		t.Errorf("cancel_job = %+v, want nothing to cancel", canceled)
	}
}

func TestCancelOverProtocol(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	cs := connect(t, f)
	started := structured[StartReportOutput](t, call(t, cs, "start_report", map[string]any{"owner": "acme", "repo": "widgets"}))
	for range 2 {
		out := structured[CancelJobOutput](t, call(t, cs, "cancel_job", map[string]any{"job_id": started.JobID}))
		if !out.CancelRequested || out.Status != statusRunning {
			t.Errorf("cancel_job = %+v, want a cancellation request", out)
		}
	}
	if len(f.cancels) != 1 {
		t.Errorf("canceled %d times, want once", len(f.cancels))
	}
}

func TestToolErrorsOverProtocol(t *testing.T) {
	t.Parallel()

	cs := connect(t, newFakeClient())
	tests := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{name: "missing required argument", tool: "start_report", args: map[string]any{"owner": "acme"}, want: "repo"},
		{name: "unknown argument", tool: "start_report", args: map[string]any{"owner": "acme", "repo": "w", "cursor": "x"}, want: "cursor"},
		{name: "wrong type", tool: "start_report", args: map[string]any{"owner": "acme", "repo": 7}, want: "repo"},
		{name: "invalid window", tool: "start_report", args: map[string]any{"owner": "acme", "repo": "w", "since": "soon"}, want: `since "soon" is not a date or time`},
		{name: "malformed job ID", tool: "job_status", args: map[string]any{"job_id": "1234"}, want: `"1234" is not a job ID`},
		{name: "unknown job", tool: "job_result", args: map[string]any{"job_id": testJobID}, want: "no job with ID"},
	}
	for _, tt := range tests {
		res := call(t, cs, tt.tool, tt.args)
		if !res.IsError || !strings.Contains(text(res), tt.want) {
			t.Errorf("%s: %s result = error %v, %q; want a tool error mentioning %q", tt.name, tt.tool, res.IsError, text(res), tt.want)
		}
	}
}
