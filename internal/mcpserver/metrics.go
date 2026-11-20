package mcpserver

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// Metrics the server records. The operation duration follows
// OpenTelemetry's semantic conventions for MCP.
const (
	// MetricOperationDuration is a histogram, in seconds, of the requests
	// and notifications the server handles, by AttrMethodName,
	// AttrToolName (tool calls only), and AttrErrorType (failures only).
	MetricOperationDuration = "mcp.server.operation.duration"

	// MetricJobsStarted counts the jobs that start_report started; a call
	// that returns an existing job for its idempotency key does not count.
	MetricJobsStarted = "durable_mcp.jobs.started"
)

// Metric attributes.
const (
	AttrMethodName = "mcp.method.name"
	AttrToolName   = "gen_ai.tool.name"
	AttrErrorType  = "error.type"
)

const (
	// errorTypeToolError is AttrErrorType for a tool call whose result is a
	// tool error, as the semantic conventions name it.
	errorTypeToolError = "tool_error"

	// otherValue stands in for a method or tool name the server does not
	// know, and for an error that is not a JSON-RPC one, so that a client
	// cannot create a time series per name it sends.
	otherValue = "_OTHER"
)

// toolNames are the tools the server has: the values AttrToolName takes,
// besides otherValue.
var toolNames = []string{"start_report", "job_status", "job_result", "cancel_job"}

// methodNames are the MCP methods AttrMethodName takes, besides
// otherValue: those of the 2025-11-25 specification that a client sends.
var methodNames = []string{
	"initialize", "ping",
	"tools/list", "tools/call",
	"resources/list", "resources/templates/list", "resources/read", "resources/subscribe", "resources/unsubscribe",
	"prompts/list", "prompts/get",
	"completion/complete", "logging/setLevel",
	"notifications/initialized", "notifications/cancelled", "notifications/progress", "notifications/roots/list_changed",
	"tasks/get", "tasks/list", "tasks/cancel", "tasks/result",
}

// operationBuckets are the bucket boundaries, in seconds, that the
// semantic conventions advise for MCP operation durations.
var operationBuckets = []float64{0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}

// instruments are the server's metric instruments.
type instruments struct {
	duration    metric.Float64Histogram
	jobsStarted metric.Int64Counter
}

// newInstruments creates the instruments from mp, or from a no-op meter
// provider if mp is nil.
func newInstruments(mp metric.MeterProvider) instruments {
	if mp == nil {
		mp = noop.NewMeterProvider()
	}
	m := mp.Meter("github.com/nicogranuja/durable-mcp/internal/mcpserver")
	var inst instruments
	var err error
	inst.duration, err = m.Float64Histogram(MetricOperationDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of the MCP requests and notifications the server handles."),
		metric.WithExplicitBucketBoundaries(operationBuckets...))
	if err != nil {
		inst.duration, _ = noop.NewMeterProvider().Meter("").Float64Histogram(MetricOperationDuration)
	}
	inst.jobsStarted, err = m.Int64Counter(MetricJobsStarted,
		metric.WithUnit("{job}"),
		metric.WithDescription("Report jobs that start_report started."))
	if err != nil {
		inst.jobsStarted, _ = noop.NewMeterProvider().Meter("").Int64Counter(MetricJobsStarted)
	}
	return inst
}

// withMetrics returns receiving middleware that records how long each
// request takes in MetricOperationDuration. It is the outermost
// middleware, so the duration includes the tool call deadline's answer.
func withMetrics(inst instruments) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			start := time.Now()
			res, err := next(ctx, method, req)
			attrs := make([]attribute.KeyValue, 0, 3)
			attrs = append(attrs, attribute.String(AttrMethodName, boundedName(methodNames, method)))
			if method == methodCallTool {
				attrs = append(attrs, attribute.String(AttrToolName, boundedName(toolNames, toolName(req))))
			}
			if errType := operationErrorType(res, err); errType != "" {
				attrs = append(attrs, attribute.String(AttrErrorType, errType))
			}
			// The request's context may be canceled already; recording
			// does not need it to be live.
			inst.duration.Record(context.WithoutCancel(ctx), time.Since(start).Seconds(), metric.WithAttributes(attrs...))
			return res, err
		}
	}
}

// boundedName returns name if it is one of known, and otherValue if not.
func boundedName(known []string, name string) string {
	if slices.Contains(known, name) {
		return name
	}
	return otherValue
}

// operationErrorType returns AttrErrorType for a request's outcome, or ""
// if it succeeded: the JSON-RPC error code of a protocol error, as the
// semantic conventions advise, or tool_error for a tool error.
func operationErrorType(res mcp.Result, err error) string {
	if err != nil {
		if wire, ok := errors.AsType[*jsonrpc.Error](err); ok {
			return strconv.FormatInt(wire.Code, 10)
		}
		return otherValue
	}
	if r, ok := res.(*mcp.CallToolResult); ok && r.IsError {
		return errorTypeToolError
	}
	return ""
}
