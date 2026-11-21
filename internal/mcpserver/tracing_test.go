package mcpserver

import (
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestToolCallSpans(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	cs := connectWith(t, newFakeClient(), Options{TracerProvider: tp})

	args := map[string]any{"owner": "acme", "repo": "widgets", "idempotency_key": "tracing-test-key"}
	call(t, cs, "start_report", args)
	if res := call(t, cs, "job_status", map[string]any{"job_id": "not-a-job-id"}); !res.IsError {
		t.Fatal("job_status of a malformed ID did not fail")
	}
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "made_up_tool"}); err == nil {
		t.Fatal("calling an unknown tool did not fail")
	}

	type want struct {
		attrs  map[string]string
		status codes.Code
	}
	wants := map[string]want{
		"tools/call start_report": {map[string]string{
			AttrMethodName: "tools/call", AttrToolName: "start_report", AttrOperationName: "execute_tool",
		}, codes.Unset},
		"tools/call job_status": {map[string]string{
			AttrMethodName: "tools/call", AttrToolName: "job_status", AttrErrorType: errorTypeToolError,
		}, codes.Error},
		"tools/call _OTHER": {map[string]string{
			AttrMethodName: "tools/call", AttrToolName: otherValue, AttrErrorType: strconv.Itoa(jsonrpc.CodeInvalidParams),
		}, codes.Error},
	}
	seen := map[string]bool{}
	for _, s := range exp.GetSpans() {
		got := map[string]string{}
		for _, kv := range s.Attributes {
			got[string(kv.Key)] = kv.Value.AsString()
			for _, leak := range []string{"acme", "widgets", "tracing-test-key", "made_up", "not-a-job-id"} {
				if strings.Contains(kv.Value.AsString(), leak) {
					t.Errorf("span %q: %s=%q holds request data", s.Name, kv.Key, kv.Value.AsString())
				}
			}
		}
		if strings.Contains(s.Status.Description+s.Name, "made_up") {
			t.Errorf("span %q, status %q: holds the unknown tool's name", s.Name, s.Status.Description)
		}
		w, ok := wants[s.Name]
		if !ok {
			continue
		}
		seen[s.Name] = true
		if s.SpanKind != trace.SpanKindServer || s.Status.Code != w.status {
			t.Errorf("span %q: kind %v, status %v, want server, %v", s.Name, s.SpanKind, s.Status.Code, w.status)
		}
		for k, v := range w.attrs {
			if got[k] != v {
				t.Errorf("span %q: %s = %q, want %q", s.Name, k, got[k], v)
			}
		}
		if _, ok := w.attrs[AttrErrorType]; !ok && got[AttrErrorType] != "" {
			t.Errorf("span %q: error.type = %q, want none", s.Name, got[AttrErrorType])
		}
	}
	for name := range wants {
		if !seen[name] {
			t.Errorf("no span %q", name)
		}
	}
}
