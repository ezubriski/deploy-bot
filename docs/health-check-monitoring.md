# Post-Deploy Health Check Monitoring

## Overview

deploy-bot can monitor application health after a deploy merges by querying
external metrics providers. If an app defines one or more health checks,
the bot polls the configured provider(s) for a configurable window after
merge and reports results in the deploy Slack thread. If the app is
unhealthy at the end of the window, a **Roll back** button is offered.

The first supported provider is **Dynatrace** (Grail / DQL API) with
OAuth2 client credentials (SSO). The architecture is provider-agnostic:
adding Datadog, New Relic, or any other metrics backend requires
implementing a single Go interface.

## How it works

```
PR merged ──► worker launches health monitor (background goroutine)
                        │
                        ├──► posts initial status message (threaded)
                        │
              ┌─────────▼──────────┐
              │  poll loop (30s)   │  ◄── configurable
              │                    │
              │  for each check:   │
              │    query provider  │
              │    evaluate threshold
              │                    │
              │  update Slack msg  │
              └────────┬───────────┘
                       │
              after poll_duration (5m default)
                       │
              ┌────────▼───────────┐
              │  all healthy?      │
              │  yes → ✅ passed   │
              │  no  → ❌ failed   │
              │        + rollback  │
              │          button    │
              └────────────────────┘
```

1. After a successful merge (manual approval or ECR auto-deploy), the
   worker checks if the app has `health_checks` configured.
2. A background goroutine posts an initial status message threaded under
   the deploy notification.
3. Every `poll_interval` (default 30s), each check queries its provider
   and evaluates the threshold. The status message is **updated in place**
   (not spammed) with the latest result for each check.
4. After `poll_duration` (default 5m), the final evaluation runs:
   - **All checks pass** (AND logic): status updated to "passed".
   - **Any check fails**: status updated to "failed" and a separate
     message with a **Roll back** button is posted.
5. The rollback button opens the standard deploy modal in rollback mode
   with the previous known-good tag pre-filled. It goes through the
   normal approval flow (authorization, lock, tag validation).

Health monitoring is fire-and-forget from the deploy handler's
perspective. If the worker restarts mid-monitoring, the in-progress
health check is lost — this is acceptable since the deploy already
merged and the feature is observational.

## Configuration

### Provider config (`config.json`)

```json
{
  "health_check": {
    "poll_interval": "30s",
    "poll_duration": "5m",
    "dynatrace": {
      "environment_url": "https://abc12345.apps.dynatrace.com",
      "token_url": "https://sso.dynatrace.com/sso/oauth2/token",
      "scopes": ["storage:metrics:read", "storage:events:read"]
    }
  }
}
```

| Field | Default | Description |
|---|---|---|
| `health_check.poll_interval` | `"30s"` | How often to query providers during the monitoring window |
| `health_check.poll_duration` | `"5m"` | Total monitoring window after merge |
| `health_check.dynatrace.environment_url` | | Base URL of the Dynatrace environment |
| `health_check.dynatrace.token_url` | | OAuth2 token endpoint for client credentials |
| `health_check.dynatrace.scopes` | `["storage:metrics:read", "storage:events:read"]` | OAuth2 scopes requested |

### Per-app health checks

Each app can define one or more health checks in its `health_checks` array.
All checks must pass (AND logic) for the app to be considered healthy.

```json
{
  "app": "myapp",
  "environment": "prod",
  "kustomize_path": "apps/myapp/overlays/prod/kustomization.yaml",
  "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/myapp",
  "health_checks": [
    {
      "provider": "dynatrace",
      "name": "response time",
      "query": "fetch dt.metrics | filter metric.key == \"service.response.time\" | filter entity.name == \"myapp-prod\" | summarize avg(value)",
      "threshold": "< 500"
    },
    {
      "provider": "dynatrace",
      "name": "error rate",
      "query": "fetch dt.metrics | filter metric.key == \"service.errors.rate\" | filter entity.name == \"myapp-prod\" | summarize avg(value)",
      "threshold": "< 0.05"
    }
  ]
}
```

| Field | Required | Description |
|---|---|---|
| `provider` | Yes | Metrics backend to query. Must match a configured provider in `health_check` (currently `"dynatrace"`) |
| `name` | No | Human-readable label shown in Slack status messages. Defaults to the provider name |
| `query` | Yes | Provider-specific query string. For Dynatrace, a DQL expression |
| `threshold` | Yes | Comparison expression applied to the first numeric value returned. Supported operators: `>`, `>=`, `<`, `<=`, `==`, `!=`. The check is healthy when the expression is true |

### Secrets

Add to the bot's secret (file or Secrets Manager):

| Field | Required | Description |
|---|---|---|
| `dynatrace_client_id` | When Dynatrace configured | OAuth2 client ID for Dynatrace SSO |
| `dynatrace_client_secret` | When Dynatrace configured | OAuth2 client secret for Dynatrace SSO |

Example (Kubernetes secret):

```bash
kubectl create secret generic deploy-bot-worker-secrets \
  --namespace=deploy-bot \
  --from-literal=secrets.json='{
    "slack_bot_token":            "xoxb-...",
    "github_token":               "github_pat_...",
    "redis_addr":                 "redis:6379",
    "postgres_password":          "changeme",
    "dynatrace_client_id":        "dt0s02.XXXXXXXX",
    "dynatrace_client_secret":    "dt0s02.XXXXXXXX.YYYYYYYY"
  }'
```

## Validation

Config validation at load time catches:

- `health_check.dynatrace` with missing `environment_url` or `token_url`
- App `health_checks` referencing a provider that isn't configured
- Missing `query` or `threshold` on any health check entry
- Invalid `poll_interval` or `poll_duration` duration strings

Secrets are validated at bot startup: if `health_check.dynatrace` is
configured but `dynatrace_client_id` or `dynatrace_client_secret` is
missing, the bot fails to start with a clear error.

## Writing DQL queries for health checks

The health check evaluator extracts the **first numeric value** from the
first record returned by the query. Queries should be written to return
a single aggregated value:

```
fetch dt.metrics
| filter metric.key == "service.response.time"
| filter entity.name == "myapp-prod"
| summarize avg(value)
```

If the query returns multiple numeric fields, the one used is
nondeterministic (Go map iteration). Stick to single-value aggregations.

The query runs with a default timeframe of `now()-5m` to `now()` on each
poll. This means each check evaluates the most recent 5 minutes of data,
not a cumulative window.

## Threshold expressions

Thresholds are simple comparison expressions:

| Expression | Healthy when |
|---|---|
| `> 0.95` | Value is greater than 0.95 |
| `>= 100` | Value is at least 100 |
| `< 500` | Value is less than 500 |
| `<= 0.01` | Value is at most 0.01 |
| `== 0` | Value is exactly 0 |
| `!= 0` | Value is not 0 |

## ECR auto-deploy behavior

For ECR-triggered auto-deploys, health checks work the same way. Since
auto-deploys don't have a deploy-request message with buttons, the health
check status posts as a top-level message in the deploy channel rather
than threaded. This can be noisy if auto-deploys are frequent — consider
whether health checks on auto-deploy apps are warranted, or use threading
via `slack.thread_threshold`.

## Extending to other providers

The health check framework is built around the `MetricsQuerier` interface:

```go
type MetricsQuerier interface {
    Query(ctx context.Context, query string) (*QueryResult, error)
}

type QueryResult struct {
    Value float64
    OK    bool
}
```

To add a new provider (e.g. Datadog):

1. Add a `DatadogConfig` struct to `internal/config/config.go` and a
   field on `HealthCheckConfig`.
2. Register the provider name in `ConfiguredProviders()`.
3. Implement the client in a new `internal/datadog/` package.
4. Add the adapter and wiring in `cmd/bot/main.go` (same pattern as
   `dynatraceAdapter`).
5. Add any required secrets fields.

No changes to the monitor, threshold evaluator, or bot integration
are needed.
