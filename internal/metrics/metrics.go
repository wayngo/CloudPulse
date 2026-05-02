// Package metrics wires up the OpenTelemetry SDK with a Prometheus exporter
// and exposes the application's custom instruments.
package metrics

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const (
	meterName = "github.com/wayne-ngo/cloudpulse"
	// ServiceName identifies this service in metric resource attributes.
	ServiceName = "cloudpulse"
)

// Instruments bundles the custom metric instruments used across the app.
// A single struct keeps wiring tidy and avoids package-level globals.
type Instruments struct {
	PingsTotal       metric.Int64Counter
	LatencyHistogram metric.Float64Histogram
}

// Provider owns the SDK MeterProvider and exposes a Shutdown hook.
type Provider struct {
	mp          *sdkmetric.MeterProvider
	Instruments *Instruments
}

// NewProvider constructs an OpenTelemetry MeterProvider backed by a
// Prometheus exporter. The returned Provider is also installed as the global
// otel meter provider so any third-party libraries pick it up automatically.
//
// The Prometheus exporter is registered against the default prometheus
// Registerer; mount promhttp.Handler() on /metrics in the HTTP layer.
func NewProvider(serviceVersion string) (*Provider, error) {
	exporter, err := prometheus.New()
	if err != nil {
		return nil, fmt.Errorf("create prometheus exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(ServiceName),
			semconv.ServiceVersion(serviceVersion),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	instr, err := newInstruments(mp.Meter(meterName))
	if err != nil {
		return nil, err
	}

	return &Provider{mp: mp, Instruments: instr}, nil
}

// Shutdown flushes and tears down the MeterProvider. Always call from main
// via defer so in-flight measurements are exported before exit.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.mp == nil {
		return nil
	}
	return p.mp.Shutdown(ctx)
}

func newInstruments(meter metric.Meter) (*Instruments, error) {
	pingsTotal, err := meter.Int64Counter(
		"total_pings_count",
		metric.WithDescription("Total number of URL pings performed, partitioned by status."),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, fmt.Errorf("create total_pings_count: %w", err)
	}

	latencyHist, err := meter.Float64Histogram(
		"latency_histogram",
		metric.WithDescription("Distribution of URL ping latencies."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return nil, fmt.Errorf("create latency_histogram: %w", err)
	}

	return &Instruments{
		PingsTotal:       pingsTotal,
		LatencyHistogram: latencyHist,
	}, nil
}
