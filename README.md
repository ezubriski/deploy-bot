# deploy-bot

> This project was largely built with [Claude Code](https://claude.ai/code). Human contributors are welcome.

A Slack bot that gates Kubernetes deployments behind an approval workflow. Developers request deployments via `/deploy` or `@bot deploy`, approvers approve or reject with Slack buttons, and the bot creates and merges GitHub PRs that update kustomize image tags in a GitOps repo. Argo CD picks up merged PRs and deploys.

Built for organizations running Kubernetes + Argo CD that want centralized, auditable deployment control without leaving Slack.

## Table of contents

- [Why deploy-bot](#why-deploy-bot)
- [Architecture](#architecture)
- [Security](#security)
- [Networking](#networking)
- [Getting started](#getting-started)
- [Commands](#commands)
- [Terraform module](#terraform-module)
- [ElastiCache example](#elasticache-example)
- [deploy-bot-config CLI](#deploy-bot-config-cli)
- [Endpoints](#endpoints)
- [Development](#development)
- [Monitoring](#monitoring)
- [Example](#example)
- [CI](#ci)
- [Further reading](#further-reading)

## Why deploy-bot

🔒 **No public network exposure.** Socket Mode (outbound WebSocket) and SQS long-polling. No ingress, no webhooks, no load balancer. Deploy it in a private subnet and forget about it.

📦 **ECR push-triggered deploys.** One EventBridge rule captures all ECR pushes account-wide. The bot filters by app and tag pattern. A single push triggers deploys for all apps sharing that ECR repo. Add a new app and it works immediately:
- No EventBridge changes
- No GitHub webhooks
- No per-repo CI pipelines

🔋 **Batteries included.** Getting started is config, not code:
- Terraform module for IAM, SQS, and EventBridge
- Kustomize base for Kubernetes manifests
- Slack app manifest for one-click app setup
- GitHub Action and CLI for config validation

⚙️ **Simple app configuration.** Define apps in `config.json` and the bot picks them up on the next hot-reload (30s poll or SIGHUP). ConfigMaps are mounted as directories so Kubernetes updates them in place — no pod restart needed. For self-service, optional [repo-sourced discovery](docs/repo-sourced-app-discovery.md) lets app teams drop a `.deploy-bot.json` in their repo — the bot discovers it, validates it, and starts deploying with no operator intervention.

📐 **Convention over configuration.** With [enforced naming conventions](docs/naming-conventions.md), app names and kustomize paths are derived from repository names. Teams only specify their environment and ECR repo — everything else follows the org standard. Conflicts between teams are structurally impossible, and onboarding a new app takes two lines of JSON.

🛡️ **Built for resilience.** The bot handles the rough edges of distributed systems:
- Redis Streams consumer groups for exactly-once processing
- In-memory buffer with backpressure during Redis outages
- Sweeper for expired deploys
- Automatic rebase on merge conflicts
- GitHub reconciliation after data loss

📈 **Horizontal scaling.** Receiver and worker scale independently. Consumer groups ensure each event processes once. Distributed Redis locks coordinate singleton work across replicas. User events and ECR events use separate Redis streams (`user:events` and `ecr:events`), so user button clicks and slash commands are never delayed by ECR bulk processing.

🔑 **Least-privilege IAM.** Separate roles and policies for bot and receiver. The Terraform module handles it. IRSA is optional.

## Architecture

```
Developer          Receiver          Redis Stream       Worker            GitHub / Argo CD
    |                  |                   |               |                     |
    |-- /deploy ------>|                   |               |                     |
    |   @bot deploy    |-- enqueue ------->|               |                     |
    |                  |<-- ack -----------|               |                     |
    |                  |                   |-- event ----->|                     |
    |                  |                   |               |-- create PR ------->|
    |                  |                   |               |                     |
Approver               |                   |               |                     |
    |-- Approve ------>|                   |               |                     |
    |                  |-- enqueue ------->|               |                     |
    |                  |                   |-- event ----->|                     |
    |                  |                   |               |-- merge PR -------->|
    |                  |                   |               |                     |-- deploy
    |                  |                   |               |                     |
ECR Push               |                   |               |                     |
    |  EventBridge --->|                   |               |                     |
    |  (SQS)           |-- enqueue ------->| ecr:events    |                     |
    |                  |                   |-- event ----->|                     |
    |                  |                   |               |-- create PR ------->|
    |                  |                   |               |   (auto-merge or    |
    |                  |                   |               |    request approval)|
```

Two processes share a single container image:

- **receiver** -- connects to Slack via Socket Mode, validates incoming events, and enqueues them to a Redis Stream. User events (slash commands, button clicks, mentions) go to `user:events`; ECR push events go to `ecr:events`. Also polls SQS for ECR push events and scans repos for app config (when enabled). Stateless; run 2+ replicas.
- **worker** -- consumes events from both streams, prioritizing `user:events` (drains it before checking `ecr:events`). Runs all business logic (GitHub API, ECR, audit logging). Run 2+ replicas; Redis Streams consumer groups ensure each event is processed once. When multiple deploys target the same environment, the bot threads individual approval requests under a parent message to reduce channel noise (configurable via `slack.thread_threshold`).

### Redis

deploy-bot relies heavily on Redis for event streaming, deploy state, per-app locks, and history. **Redis must be available for the bot to function.** For production, use ElastiCache for Redis (or an equivalent managed service) with Multi-AZ automatic failover and AOF persistence enabled. This gives you durability across restarts and high availability during node failures.

The bot tolerates brief Redis connectivity interruptions -- the in-memory buffer absorbs events during outages and replays them on reconnection. It also recovers from complete Redis data loss by reconciling against GitHub (closing orphaned PRs, releasing stale locks). However, performance degrades during outages and deploy requests will queue rather than process.

An in-cluster Redis deployment (e.g. the `redis.yaml` in `deploy/`) works for development and low-volume environments, but lacks the persistence and failover guarantees that matter when deployments are business-critical. See [docs/redis-resilience.md](docs/redis-resilience.md) for detailed behaviour during outages and recovery.

## Security

- **Minimal container image** -- built `FROM scratch`. No shell, no package manager, no OS. Just the binary and CA certificates.
- **Hardened runtime** -- runs as non-root (UID 65534), read-only filesystem, all capabilities dropped, seccomp RuntimeDefault.
- **No inbound network** -- Socket Mode uses an outbound WebSocket. ECR events arrive via SQS long-poll. Nothing listens on a public port.
- **Secrets isolation** -- tokens and credentials loaded from AWS Secrets Manager (or a Kubernetes Secret volume mount). Never stored in config files.
- **Deploy locks** -- per-app, per-environment locks prevent concurrent deploys to the same target.
- **Identity verification** -- Slack user ID is resolved to email (Slack API), then to GitHub login (GitHub API), then checked against team membership. Every action is traced back to a verified identity.
- **Input sanitization** -- user-provided text (deploy reasons, rejection reasons) is sanitized before rendering in Slack messages and GitHub comments to prevent injection.
- **Tag validation** -- image tags are validated against an allowlist regex before use in branch names, YAML files, and git refs.

## Networking

deploy-bot requires no ingress controller, load balancer, or public IP. All external communication is outbound:

| Direction | Protocol | Destination | Purpose |
|---|---|---|---|
| Outbound | WSS | `wss://wss-primary.slack.com` | Slack Socket Mode (receiver) |
| Outbound | HTTPS | `sqs.{region}.amazonaws.com` | ECR event polling (receiver) |
| Outbound | HTTPS | `api.github.com` | PR creation, merge, close (worker) |
| Outbound | HTTPS | `api.ecr.{region}.amazonaws.com` | Tag listing and cache refresh (worker) |
| Outbound | HTTPS | `s3.{region}.amazonaws.com` | Audit log writes (worker, optional) |
| Outbound | HTTPS | `slack.com` | Slack Web API calls (worker) |
| Internal | TCP 6379 | Redis | State, locks, streams, history |
| Internal | HTTP | Inter-pod | Health checks (`/healthz`, `/readyz`) |

If deployed on AWS with VPC endpoints for SQS, ECR, S3, and Secrets Manager, the bot can run in a fully private subnet with no internet gateway. The only service that requires public internet access is the Slack Socket Mode WebSocket and the Slack/GitHub APIs.

## Getting started

Two paths depending on your goals:

| Guide | Time | What you get |
|---|---|---|
| **[Quickstart](docs/quickstart.md)** | ~30 min | IRSA roles, in-cluster Redis, no ECR events. Kick the tires, run `/deploy`, see it work. |
| **[Production setup](docs/production-setup.md)** | ~1 hour | IRSA, ElastiCache IAM auth, WORM audit bucket, CMK encryption, ECR push deploys, repo discovery. Designed with compliance in mind. |

Start with the quickstart. Move to the production guide when you're ready to harden.

## Commands

App names include the environment suffix (e.g. `myapp-dev`, `myapp-prod`). Use `apps` to list configured apps.

### Slash commands

| Command | Description |
|---|---|
| `/deploy` | Open the deployment request modal |
| `/deploy <app-env>` | Open the modal pre-selected to an app |
| `/deploy list` | List all pending deployments (alias: `status`) |
| `/deploy history [app-env]` | Show recent completed deployments |
| `/deploy apps` | List all configured apps and their source (operator or repo) |
| `/deploy conflicts` | List repo-sourced apps blocked by operator config |
| `/deploy tags <app-env>` | List the 20 most recent valid tags for an app |
| `/deploy tags <app-env> <tag>` | Verify a specific tag exists in ECR |
| `/deploy cancel <pr>` | Cancel your own pending deployment |
| `/deploy nudge <pr>` | Re-ping the approver |
| `/deploy rollback <app-env>` | Re-deploy the previous approved tag |
| `/deploy help` | Show command help |

### @mention commands

All commands are available by mentioning the bot in any channel:

| Command | Description |
|---|---|
| `@bot deploy <app-env> <tag> [@approver] [reason]` | Create a deploy PR with positional args |
| `@bot list` | List pending deployments (alias: `status`) |
| `@bot history [app-env]` | Show recent deploys |
| `@bot apps` | List configured apps |
| `@bot conflicts` | List config conflicts |
| `@bot tags <app-env>` | List recent tags |
| `@bot cancel <pr>` | Cancel your own deployment |
| `@bot nudge <pr>` | Remind the approver |
| `@bot rollback <app-env>` | Deploy the previous tag |
| `@bot help` | Show command help |

Mention responses are posted in-channel (threaded if the mention was in a thread). The slash command provides a guided modal with dropdowns and validation.

## Terraform module

The `terraform/` directory contains a module that creates all AWS resources:

- Separate IAM roles for bot (worker) and receiver -- least-privilege by default
- IAM policies exported as managed policies (`bot_policy_arn`, `receiver_policy_arn`) so they work with IAM users when IRSA is not available
- SQS queue and EventBridge rule for ECR push-triggered deploys (opt-in via `ecr_events_enabled`)
- Optional `permissions_boundary` support

The module also supports Secrets Manager secret creation, S3 audit buckets with WORM compliance, SQS/audit bucket encryption with customer-managed KMS keys, and ElastiCache IAM auth permissions. See [terraform/README.md](terraform/README.md) for the full variable and output reference.

## ElastiCache example

A reference ElastiCache module is provided at [`terraform/examples/elasticache/`](terraform/examples/elasticache/) with recommended settings: IAM authentication, encryption in transit and at rest, multi-AZ failover, and automatic snapshots. It is provided as an example only and is not actively maintained — copy it into your own infrastructure and adapt as needed. See the [production setup guide](docs/production-setup.md) for how to wire it together with the deploy-bot module.

## deploy-bot-config CLI

A standalone binary for validating deploy-bot configuration files. Validates the main `config.json` (required fields, duration parsing, regex compilation, duplicate detection) and repo-sourced `.deploy-bot.json` files.

**Usage:**

```bash
# Validate the main config.json
deploy-bot-config validate --config config.json

# Validate a repo-sourced .deploy-bot.json
deploy-bot-config validate --file .deploy-bot.json

# JSON output (for CI)
deploy-bot-config validate --config config.json --format json
```

**Exit codes:**

| Code | Meaning |
|---|---|
| 0 | Config is valid |
| 1 | Validation errors found |
| 2 | File not found or JSON parse error |

**Main config validation** checks required fields, duration formats (`stale_duration`, `lock_ttl`), merge method, tag pattern regex, duplicate app+environment pairs, and ECR region:

```
$ deploy-bot-config validate --config config.json
config.json

  ✓ github.org
  ✓ github.repo
  ✓ authorization (at least one entry)
  ✓ slack.deploy_channel
  ✓ deployment.stale_duration
  ✓ deployment.lock_ttl
  ✓ deployment.merge_method
  ✓ aws.ecr_region

  ✓ apps[0] myapp (dev)
  ✓ apps[1] myapp (prod)

2/2 apps valid.

Config is valid.
```

**Repo config validation** checks `.deploy-bot.json` files that app teams create for repo-sourced discovery:

```
$ deploy-bot-config validate --file .deploy-bot.json
.deploy-bot.json (deploy-bot/v1)

  ✓ apps[0] (myapp-dev): ok
  ✗ apps[1] (broken): kustomize_path: required

1/2 apps valid. 1 error found.
```

### GitHub Action

A reusable GitHub Action is provided at `.github/actions/validate-config/`. Add it to your repo's CI:

```yaml
- uses: ezubriski/deploy-bot/.github/actions/validate-config@main
  with:
    config-file: .deploy-bot.json  # default
```

The action builds the validator from source and runs it against your config file. Failures block the PR.

## Endpoints

| Process | Port | Paths |
|---|---|---|
| worker | 9090 | `/healthz` (liveness), `/readyz` (readiness), `/metrics` (Prometheus) |
| receiver | 8080 | `/healthz` (liveness) |

## Development

```bash
make build              # build all binaries (bot, receiver, deploy-bot-config) to ./bin
make test               # run unit tests (uses miniredis, no external deps)
make test-pkg PKG=./internal/store/...  # single package
make test-integ         # integration tests (requires .env.integration)
make test-integ-single RUN=TestDeployAndApprove  # single integration test
make lint               # golangci-lint
make image              # build container image with Podman
make push               # build and push to ECR
make push TAG=v1.2.3    # push with a specific tag
make release BUMP=minor # trigger release workflow (patch|minor|major)
make clean              # remove ./bin
```

Integration tests require a `.env.integration` file. See [docs/integration-test-setup.md](docs/integration-test-setup.md) for the full setup.

## Monitoring

Worker pods expose Prometheus metrics on port `9090` at `/metrics` and are annotated for auto-discovery:

```yaml
prometheus.io/scrape: "true"
prometheus.io/port:   "9090"
prometheus.io/path:   "/metrics"
```

For the Prometheus Operator, use a `ServiceMonitor`:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: deploy-bot-worker
  namespace: deploy-bot
spec:
  selector:
    matchLabels:
      app: deploy-bot-worker
  endpoints:
    - port: metrics
      path: /metrics
```

## Example

See the bot opening and closing PRs in a live gitops repo: [aFakeBusiness/gitops](https://github.com/aFakeBusiness/gitops)

## CI

GitHub Actions runs on pull requests and pushes to `main` and version tags (`v*`). Lint, test, and build fire in parallel on self-hosted runners:

1. **Lint** -- `golangci-lint run`
2. **Test** -- `make test` (unit tests with `-race`; no external dependencies)
3. **Build and push** -- builds with Podman and pushes to ghcr.io tagged with the short commit SHA. Also pushes to ECR if the `ECR_REGISTRY` repository secret is set.
4. **Promote** (after all three pass) -- tags the SHA image as `latest`, and with the semver tag on `v*` pushes.

## Further reading

- [Quickstart](docs/quickstart.md)
- [Production setup](docs/production-setup.md)
- [Configuration reference](docs/configuration.md)
- [ECR push-triggered deploys](docs/ecr-push-triggered-deploys.md)
- [Repo-sourced app discovery](docs/repo-sourced-app-discovery.md)
- [Naming conventions and conflict resolution](docs/naming-conventions.md)
- [No-op deploy handling](docs/no-op-deploy-handling.md)
- [Merge conflict handling](docs/merge-conflict-handling.md)
- [Redis resilience](docs/redis-resilience.md)
- [Integration test setup](docs/integration-test-setup.md)
- [Terraform module](terraform/README.md)
- [ElastiCache example](terraform/examples/elasticache/)
