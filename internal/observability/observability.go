// Package observability sets up the OpenTelemetry MeterProvider used by
// deploy-bot for HTTP and Redis instrumentation.
//
// Setup installs the MeterProvider as the OTEL global. After that, any code
// in the program can call the package-level helpers (WrapTransport,
// HTTPClient, InstrumentAWSConfig, InstrumentRedis) to opt in to
// instrumentation without plumbing a *Provider through every constructor.
//
// The Prometheus exporter writes into the registerer passed to Setup
// (normally prometheus.DefaultRegisterer), so /metrics continues to expose
// existing client_golang metrics and OTEL metrics from one endpoint.
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
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/prometheus/client_golang/prometheus"
	redisotel "github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Provider owns the MeterProvider so the caller can shut it down on exit.
// All instrumentation helpers in this package read from the OTEL global
// rather than this struct.
type Provider struct {
	mp *sdkmetric.MeterProvider
}

// Setup initializes a MeterProvider, installs it as the OTEL global, and
// registers a Prometheus exporter against reg. serviceName is recorded as
// the service.name resource attribute.
func Setup(serviceName string, reg prometheus.Registerer) (*Provider, error) {
	exporter, err := otelprom.New(otelprom.WithRegisterer(reg))
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

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)
	return &Provider{mp: mp}, nil
}

// Shutdown flushes and stops the MeterProvider. Safe on a nil receiver.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	return p.mp.Shutdown(ctx)
}

// WrapTransport wraps an existing RoundTripper with otelhttp instrumentation.
// Reads the meter provider from the OTEL global, so Setup must have been
// called first for metrics to flow.
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
