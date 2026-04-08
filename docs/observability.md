# Observability

deploy-bot is instrumented with [OpenTelemetry](https://opentelemetry.io/).
Both `cmd/bot` and `cmd/receiver` set up an OTEL `MeterProvider` at startup
and install it as the OTEL global, so library-aware OTEL contrib
instrumentations attached at construction sites flow into it automatically.

## What gets instrumented

| Subsystem | Library | Metrics |
|---|---|---|
| GitHub HTTP (PRs, comments, labels, members) | `otelhttp` wrapping the ghinstallation/oauth2 transports | `http.client.request.duration`, request/response body size, status codes |
| Slack HTTP | `otelhttp` via `slack.OptionHTTPClient` | same as above |
| AWS SDK calls (ECR, S3 audit, SQS) | `otelhttp` swapped into `aws.Config.HTTPClient` (otelaws is currently traces-only) | same as above, labeled by `server.address` so per-service traffic is distinguishable |
| Redis (`go-redis` v9) | `redisotel` metrics hook | `db.client.operation.duration`, connection pool stats |

The Prometheus exporter writes into the same `prometheus.DefaultRegisterer`
as the existing `client_golang` metrics, so the bot's `/metrics` endpoint
(`:9090/metrics` for the worker, `:8080/metrics` for the receiver) exposes
both deploy-bot's domain counters and the OTEL HTTP/Redis metrics from a
single endpoint. No additional scrape config needed.

## Routing telemetry elsewhere

The Prometheus metrics reader is always wired in code. In addition,
deploy-bot uses the OTEL [`autoexport`](https://pkg.go.dev/go.opentelemetry.io/contrib/exporters/autoexport)
package to honor the standard OTEL environment variables for routing
metrics and traces to a collector or to stdout. Setting any of the
variables below on the `bot` or `receiver` containers takes effect at
process start with no code changes:

| Variable | Purpose | Default in deploy-bot | Common values |
|---|---|---|---|
| `OTEL_SERVICE_NAME` | Sets the `service.name` resource attribute. | `deploy-bot` (worker) / `deploy-bot-receiver` (receiver), set in code | any string |
| `OTEL_METRICS_EXPORTER` | Adds a metrics pipeline alongside the always-on Prometheus reader. | unset → Prometheus only; we deliberately do **not** call autoexport when this is empty so the SDK does not silently push OTLP to localhost | `otlp`, `console`, `none` |
| `OTEL_TRACES_EXPORTER` | Enables a tracer provider. | unset → no tracer provider installed, no spans emitted | `otlp`, `console`, `none` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector address used by `otlp` exporters. | OTEL spec default: `http://localhost:4318` (HTTP) or `http://localhost:4317` (gRPC) | `http://otel-collector.observability:4318` |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | OTLP wire format. | OTEL spec default: `grpc` | `grpc`, `http/protobuf` |
| `OTEL_RESOURCE_ATTRIBUTES` | Extra resource attributes merged onto every signal. | none | `deployment.environment=prod,team=platform` |

OTEL log signal routing (`OTEL_LOGS_EXPORTER`) is **not** wired —
deploy-bot uses zap for application logging and does not emit OTEL log
records.

### Configuration source

The variables above are the **only** supported way to configure OTEL in
deploy-bot. The OTEL spec also defines a file-based configuration mechanism
(`OTEL_EXPERIMENTAL_CONFIG_FILE` and the YAML declarative config format);
deploy-bot does **not** wire that in. If you need to manage OTEL settings
declaratively, set the environment variables via your Kubernetes manifests
(or whatever process manager you use) — do not point deploy-bot at an OTEL
config file, it will be ignored.

> **One-off profiling tip.** To capture a baseline of external I/O
> without standing up a collector, set `OTEL_METRICS_EXPORTER=console`
> and run the integration tests (`make test-integ`). Each process writes
> its metrics to stdout; redirect to per-process files and analyze offline.

The full set of OTEL environment variables — including sampling, batching,
header injection, and per-signal endpoint overrides — is documented at
[opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/).
