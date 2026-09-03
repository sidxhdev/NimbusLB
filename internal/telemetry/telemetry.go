// Package telemetry wires OpenTelemetry tracing and metrics for NimbusLB.
package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

const serviceName = "nimbuslb"

// backendHealth stores the latest health state per backend URL for the
// observable gauge callback. Entries are written during health checks and
// read by the SDK callback; access is race-safe via sync.Map.
var backendHealth MapStringBool

// Init sets up the tracer provider (OTLP gRPC when endpoint is non-empty,
// otherwise a no-op provider) and the meter provider backed by the
// Prometheus exporter. It returns a shutdown function and the metrics
// registry used by the balancer.
func Init(ctx context.Context, otlpEndpoint string) (shutdown func(context.Context) error, m *Metrics, err error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("building resource: %w", err)
	}

	// --- Tracing ---
	var tp *sdktrace.TracerProvider
	if otlpEndpoint != "" {
		// Non-blocking dial: spans buffer and export once the collector is
		// reachable; the LB never waits on telemetry infrastructure.
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(otlpEndpoint),
			otlptracegrpc.WithInsecure(),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("otlp trace exporter: %w", err)
		}
		tp = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(2*time.Second)),
			sdktrace.WithResource(res),
		)
	} else {
		tp = sdktrace.NewTracerProvider(sdktrace.WithResource(res))
	}
	otel.SetTracerProvider(tp)

	// --- Metrics ---
	promExp, err := otelprom.New()
	if err != nil {
		return nil, nil, fmt.Errorf("prometheus exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(promExp),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	m, err = NewMetrics(mp.Meter(serviceName))
	if err != nil {
		return nil, nil, err
	}

	// Observable gauge: lb_backend_health (1 = healthy, 0 = down).
	gauge, err := mp.Meter(serviceName).Int64ObservableGauge(
		"lb_backend_health",
		metric.WithDescription("Backend health status (1 = healthy, 0 = down)"),
	)
	if err != nil {
		return nil, nil, err
	}
	_, err = mp.Meter(serviceName).RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			backendHealth.Range(func(k string, v bool) bool {
				var val int64
				if v {
					val = 1
				}
				o.ObserveInt64(gauge, val, metric.WithAttributes(attribute.String("backend", k)))
				return true
			})
			return nil
		},
		gauge,
	)
	if err != nil {
		return nil, nil, err
	}

	shutdown = func(ctx context.Context) error {
		if err := tp.Shutdown(ctx); err != nil {
			return err
		}
		return mp.Shutdown(ctx)
	}
	return shutdown, m, nil
}

// MetricsHandler serves the Prometheus scrape endpoint.
func MetricsHandler() http.Handler { return promhttp.Handler() }

// SetBackendHealth records the latest health state for a backend; consumed
// by the observable gauge on each Prometheus scrape.
func SetBackendHealth(backendURL string, alive bool) {
	backendHealth.Store(backendURL, alive)
}

// Metrics bundles all load-balancer instruments.
type Metrics struct {
	duration  metric.Float64Histogram
	requests  metric.Int64Counter
	errors    metric.Int64Counter
	inflight  metric.Int64UpDownCounter
	startNano atomic.Int64
}

// NewMetrics creates the instrument set on the given meter.
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	var err error
	m := &Metrics{}

	m.duration, err = meter.Float64Histogram(
		"lb_request_duration_ms",
		metric.WithDescription("Proxied request latency in milliseconds"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return nil, err
	}
	m.requests, err = meter.Int64Counter(
		"lb_requests_total",
		metric.WithDescription("Total proxied requests"),
	)
	if err != nil {
		return nil, err
	}
	m.errors, err = meter.Int64Counter(
		"lb_errors_total",
		metric.WithDescription("Total proxy/backend errors"),
	)
	if err != nil {
		return nil, err
	}
	m.inflight, err = meter.Int64UpDownCounter(
		"lb_requests_inflight",
		metric.WithDescription("Requests currently in flight"),
	)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// RecordRequest records latency and count for a completed proxied request.
func (m *Metrics) RecordRequest(ctx context.Context, backendURL, method, path string, status int) {
	attrs := metric.WithAttributes(
		attribute.String("backend", backendURL),
		attribute.String("method", method),
		attribute.String("path", path),
		attribute.Int("status", status),
	)
	m.requests.Add(ctx, 1, attrs)
	if start := StartTimeFromContext(ctx); start > 0 {
		m.duration.Record(ctx, float64(time.Since(time.Unix(0, start)))/float64(time.Millisecond), attrs)
	}
}

// RecordError increments the error counter.
func (m *Metrics) RecordError(ctx context.Context, errType, backendURL string) {
	m.errors.Add(ctx, 1, metric.WithAttributes(
		attribute.String("error_type", errType),
		attribute.String("backend", backendURL),
	))
}

// InflightInc/InflightDec track in-flight requests.
func (m *Metrics) InflightInc(ctx context.Context) { m.inflight.Add(ctx, 1) }
func (m *Metrics) InflightDec(ctx context.Context) { m.inflight.Add(ctx, -1) }
