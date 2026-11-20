// Package telemetrytest records metrics in memory, for tests that check
// what code records.
package telemetrytest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/nicogranuja/durable-mcp/internal/telemetry"
)

// Reader is an in-memory metric reader and the meter provider that feeds
// it, with the same views as the commands' meter provider.
type Reader struct {
	reader   *metric.ManualReader
	Provider *metric.MeterProvider
}

// New returns a Reader.
func New(t *testing.T) *Reader {
	t.Helper()
	r := metric.NewManualReader()
	mp := telemetry.NewMeterProvider(resource.Empty(), r)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return &Reader{reader: r, Provider: mp}
}

// Point is one data point of a counter or histogram.
type Point struct {
	// Attrs are the point's attributes.
	Attrs map[string]string
	// Value is a counter's sum, or a histogram's count.
	Value float64
	// Sum is a histogram's sum.
	Sum float64
	// Bounds are a histogram's bucket boundaries.
	Bounds []float64
}

// Points collects the metrics and returns the points of the named one, or
// nil if it was not recorded.
func (r *Reader) Points(t *testing.T, name string) []Point {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var points []Point
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					points = append(points, Point{Attrs: attrs(p.Attributes.ToSlice()), Value: float64(p.Value)})
				}
			case metricdata.Sum[float64]:
				for _, p := range d.DataPoints {
					points = append(points, Point{Attrs: attrs(p.Attributes.ToSlice()), Value: p.Value})
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					points = append(points, Point{
						Attrs: attrs(p.Attributes.ToSlice()), Value: float64(p.Count), Sum: p.Sum, Bounds: p.Bounds,
					})
				}
			default:
				t.Fatalf("metric %s has unexpected data %T", name, m.Data)
			}
		}
	}
	return points
}

// Find returns the sum of the values of the points whose attributes
// include want. A want value of "" means the point must not have that
// attribute.
func Find(points []Point, want map[string]string) float64 {
	var total float64
	for _, p := range points {
		if matches(p.Attrs, want) {
			total += p.Value
		}
	}
	return total
}

func matches(got, want map[string]string) bool {
	for k, v := range want {
		if g, ok := got[k]; g != v || (v == "" && ok) {
			return false
		}
	}
	return true
}

func attrs(kvs []attribute.KeyValue) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value.String()
	}
	return m
}
