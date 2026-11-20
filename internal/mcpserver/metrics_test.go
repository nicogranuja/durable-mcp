package mcpserver

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nicogranuja/durable-mcp/internal/telemetry/telemetrytest"
)

func TestOperationMetrics(t *testing.T) {
	t.Parallel()

	r := telemetrytest.New(t)
	cs := connectWith(t, newFakeClient(), Options{MeterProvider: r.Provider})

	if _, err := cs.ListTools(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"owner": "acme", "repo": "widgets", "idempotency_key": "metrics-test-key"}
	first := structured[StartReportOutput](t, call(t, cs, "start_report", args))
	// The same key again returns the job: not a second start.
	call(t, cs, "start_report", args)
	if res := call(t, cs, "job_status", map[string]any{"job_id": "not-a-job-id"}); !res.IsError {
		t.Fatal("job_status of a malformed ID did not fail")
	}
	// An unknown tool is a protocol error, and its name must not become a
	// label value.
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "made_up_tool_" + first.JobID}); err == nil {
		t.Fatal("calling an unknown tool did not fail")
	}

	points := r.Points(t, MetricOperationDuration)
	for _, tt := range []struct {
		attrs map[string]string
		want  float64
	}{
		{map[string]string{AttrMethodName: "tools/list", AttrToolName: "", AttrErrorType: ""}, 1},
		{map[string]string{AttrMethodName: "tools/call", AttrToolName: "start_report", AttrErrorType: ""}, 2},
		{map[string]string{AttrMethodName: "tools/call", AttrToolName: "job_status", AttrErrorType: errorTypeToolError}, 1},
		{map[string]string{
			AttrMethodName: "tools/call", AttrToolName: otherValue, AttrErrorType: strconv.Itoa(jsonrpc.CodeInvalidParams),
		}, 1},
	} {
		if got := telemetrytest.Find(points, tt.attrs); got != tt.want {
			t.Errorf("%s%v = %v, want %v; points %+v", MetricOperationDuration, tt.attrs, got, tt.want, points)
		}
	}
	for _, p := range points {
		if !slices.Equal(p.Bounds, operationBuckets) {
			t.Errorf("%s buckets = %v, want %v", MetricOperationDuration, p.Bounds, operationBuckets)
		}
		for k, v := range p.Attrs {
			if strings.Contains(v, first.JobID) || strings.Contains(v, "made_up") || strings.Contains(v, "acme") {
				t.Errorf("%s has attribute %s=%q, from the request", MetricOperationDuration, k, v)
			}
		}
	}

	started := r.Points(t, MetricJobsStarted)
	if got := telemetrytest.Find(started, nil); got != 1 || len(started) != 1 || len(started[0].Attrs) != 0 {
		t.Errorf("%s points = %+v, want one point of 1 without attributes", MetricJobsStarted, started)
	}
}

func TestBoundedNames(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		known    []string
		in, want string
	}{
		{toolNames, "start_report", "start_report"},
		{toolNames, "Start_Report", otherValue},
		{toolNames, "", otherValue},
		{methodNames, "tools/call", "tools/call"},
		{methodNames, "tools/call/" + strings.Repeat("x", 100), otherValue},
	} {
		if got := boundedName(tt.known, tt.in); got != tt.want {
			t.Errorf("boundedName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestOperationErrorType(t *testing.T) {
	t.Parallel()

	toolErr := &mcp.CallToolResult{IsError: true}
	for _, tt := range []struct {
		res  mcp.Result
		err  error
		want string
	}{
		{&mcp.CallToolResult{}, nil, ""},
		{toolErr, nil, errorTypeToolError},
		{nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound}, "-32601"},
		{nil, &jsonrpc.Error{Code: 123456789}, otherValue},
		{nil, errors.New("boom"), otherValue},
	} {
		if got := operationErrorType(tt.res, tt.err); got != tt.want {
			t.Errorf("operationErrorType(%v, %v) = %q, want %q", tt.res, tt.err, got, tt.want)
		}
	}
}
