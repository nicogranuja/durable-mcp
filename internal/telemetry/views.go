package telemetry

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// temporalBuckets are the bucket boundaries, in seconds, of the Temporal
// SDK's histograms. The SDK records durations in seconds without bucket
// advice, and OpenTelemetry's default boundaries (0 to 10000) suit
// milliseconds: every task latency would land in the first bucket or two.
// These span a fast workflow task (milliseconds) to a long poll (a minute).
var temporalBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120,
}

// NewMeterProvider returns a meter provider that sends to readers, with
// the views every durable-mcp meter provider needs. Tests pass a manual
// reader.
func NewMeterProvider(res *resource.Resource, readers ...metric.Reader) *metric.MeterProvider {
	opts := []metric.Option{metric.WithResource(res)}
	for _, r := range readers {
		opts = append(opts, metric.WithReader(r))
	}
	opts = append(opts, metric.WithView(views()...))
	return metric.NewMeterProvider(opts...)
}

// views set bucket boundaries for the histograms recorded through the
// Temporal SDK's metrics handler, which cannot pass bucket advice.
func views() []metric.View {
	return []metric.View{
		metric.NewView(
			metric.Instrument{Name: temporalHistogramFilter, Kind: metric.InstrumentKindHistogram},
			metric.Stream{Aggregation: metric.AggregationExplicitBucketHistogram{Boundaries: temporalBuckets}},
		),
		metric.NewView(
			metric.Instrument{Name: report.MetricRateLimitWait, Kind: metric.InstrumentKindHistogram},
			metric.Stream{Aggregation: metric.AggregationExplicitBucketHistogram{Boundaries: report.RateLimitWaitBuckets()}},
		),
	}
}

// componentAttribute is the durable_mcp.component resource attribute.
func componentAttribute(component string) attribute.KeyValue {
	return attribute.String(componentAttributeKey, component)
}
