// Package observability sets up the OpenTelemetry MeterProvider and
// TracerProvider used by deploy-bot for HTTP and Redis instrumentation.
//
// Setup installs the providers as OTEL globals. After that, any code in the
// program can call the package-level helpers (WrapTransport, HTTPClient,
// InstrumentAWSConfig, InstrumentRedis) to opt in to instrumentation without
// plumbing a *Provider through every constructor.
//
// The Prometheus exporter is always wired in for metrics so /metrics
// continues to expose existing client_golang counters and OTEL metrics from
// one endpoint. Operators can additionally route signals to OTLP, stdout,
// etc. by setting the standard OTEL environment variables — see
// docs/observability.md.
//
// This package does not "auto-instrument" anything in the Java/JS sense — Go
// has no runtime hooks for that. It provides one-liners to wire
// library-aware OTEL contrib instrumentations into existing constructors:
// otelhttp for HTTP clients (also used to instrument the AWS SDK at the
// HTTP layer, since otelaws is currently traces-only), and redisotel for
// go-redis.
package observability

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/prometheus/client_golang/prometheus"
	redisotel "github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// Provider owns the MeterProvider and TracerProvider so the caller can
// shut them down on exit. All instrumentation helpers in this package read
// from the OTEL globals rather than this struct.
type Provider struct {
	mp *sdkmetric.MeterProvider
	tp *sdktrace.TracerProvider
}

// Setup initializes the MeterProvider and (if a traces exporter is
// configured) the TracerProvider, installs them as OTEL globals, and wires
// the Prometheus metrics exporter against reg.
//
// Additional metric and trace exporters are configured from the standard
// OTEL_METRICS_EXPORTER / OTEL_TRACES_EXPORTER environment variables via
// the autoexport package, so operators can route telemetry to OTLP, stdout,
// etc. without code changes. When unset, only the Prometheus metrics
// exporter is active and no tracer provider is installed.
func Setup(serviceName string, reg prometheus.Registerer) (*Provider, error) {
	ctx := context.Background()

	promExporter, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		return nil, fmt.Errorf("create prometheus exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
	))
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	meterOpts := []sdkmetric.Option{
		sdkmetric.WithReader(promExporter),
		sdkmetric.WithResource(res),
	}

	// Honor OTEL_METRICS_EXPORTER if set. We don't call autoexport when the
	// var is unset because its default is "otlp", which would silently push
	// to localhost:4318 — surprising for an operator who hasn't opted in.
	if exporters := os.Getenv("OTEL_METRICS_EXPORTER"); exporters != "" {
		reader, err := autoexport.NewMetricReader(ctx)
		if err != nil {
			return nil, fmt.Errorf("autoexport metric reader: %w", err)
		}
		if !autoexport.IsNoneMetricReader(reader) {
			meterOpts = append(meterOpts, sdkmetric.WithReader(reader))
		}
	}

	mp := sdkmetric.NewMeterProvider(meterOpts...)
	otel.SetMeterProvider(mp)

	p := &Provider{mp: mp}

	// Tracer provider is only installed if the operator opted in via
	// OTEL_TRACES_EXPORTER. No traces are emitted by default.
	if exporters := os.Getenv("OTEL_TRACES_EXPORTER"); exporters != "" {
		spanExporter, err := autoexport.NewSpanExporter(ctx)
		if err != nil {
			return nil, fmt.Errorf("autoexport span exporter: %w", err)
		}
		if !autoexport.IsNoneSpanExporter(spanExporter) {
			tp := sdktrace.NewTracerProvider(
				sdktrace.WithBatcher(spanExporter),
				sdktrace.WithResource(res),
			)
			otel.SetTracerProvider(tp)
			p.tp = tp
		}
	}

	return p, nil
}

// Shutdown flushes and stops the MeterProvider and TracerProvider. Safe on
// a nil receiver.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var errs []error
	if p.mp != nil {
		if err := p.mp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("meter provider shutdown: %w", err))
		}
	}
	if p.tp != nil {
		if err := p.tp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("tracer provider shutdown: %w", err))
		}
	}
	return errors.Join(errs...)
}

// WrapTransport wraps an existing RoundTripper with otelhttp instrumentation.
// Reads the meter and tracer providers from the OTEL globals, so Setup must
// have been called first for telemetry to flow.
func WrapTransport(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return otelhttp.NewTransport(rt)
}

// HTTPClient returns an *http.Client whose transport is wrapped with
// otelhttp. base may be nil.
func HTTPClient(base http.RoundTripper) *http.Client {
	return &http.Client{Transport: WrapTransport(base)}
}

// InstrumentAWSConfig swaps cfg.HTTPClient for an otelhttp-wrapped client so
// every AWS API call is observed at the HTTP layer. Per-host labels
// distinguish ECR, SQS, S3, SecretsManager, etc.
func InstrumentAWSConfig(cfg *aws.Config) {
	cfg.HTTPClient = HTTPClient(nil)
}

// InstrumentRedis attaches the redisotel metrics hook to rdb.
func InstrumentRedis(rdb *redis.Client) error {
	return redisotel.InstrumentMetrics(rdb)
}
