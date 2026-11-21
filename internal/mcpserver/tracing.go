package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	// AttrOperationName is the GenAI operation of a tool call span.
	AttrOperationName = "gen_ai.operation.name"

	// operationExecuteTool is AttrOperationName for tools/call.
	operationExecuteTool = "execute_tool"

	tracerScope = "github.com/nicogranuja/durable-mcp/internal/mcpserver"
)

// withTracing returns receiving middleware that wraps each request the
// server handles in a span, following OpenTelemetry's semantic conventions
// for MCP: named "{mcp.method.name} {gen_ai.tool.name}" for a tool call,
// and the method alone otherwise, with the same bounded attributes as
// MetricOperationDuration. The Temporal client calls a tool makes, such as
// starting the workflow, become its children, and the workflow and its
// activities theirs.
//
// The span records no arguments, results, or error messages: they hold
// what callers send, such as repository names and idempotency keys, which
// can be anything. A failure sets the span's status to error, described by
// its error.type.
func withTracing(tp trace.TracerProvider) mcp.Middleware {
	if tp == nil {
		tp = noop.NewTracerProvider()
	}
	tracer := tp.Tracer(tracerScope)
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			name := boundedName(methodNames, method)
			attrs := make([]attribute.KeyValue, 0, 3)
			attrs = append(attrs, attribute.String(AttrMethodName, name))
			if method == methodCallTool {
				tool := boundedName(toolNames, toolName(req))
				name += " " + tool
				attrs = append(attrs,
					attribute.String(AttrToolName, tool),
					attribute.String(AttrOperationName, operationExecuteTool))
			}
			ctx, span := tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
			defer span.End()

			res, err := next(ctx, method, req)
			if errType := operationErrorType(res, err); errType != "" {
				span.SetAttributes(attribute.String(AttrErrorType, errType))
				span.SetStatus(codes.Error, errType)
			}
			return res, err
		}
	}
}
