# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

Use `make help` to list all targets. Common ones:

```bash
# Build both binaries to ./bin
make build

# Unit tests (no network required)
make test

# Single package
make test-pkg PKG=./internal/store/...

# Integration tests (loads .env.integration)
make test-integ

# Single integration test
make test-integ-single RUN=TestDeployAndApprove

# Lint
make lint

# Container image (Podman)
make image          # build
make push           # build + push to ECR
make ecr-login      # authenticate Podman to ECR
```

Unit tests use `miniredis` — no real Redis needed.

Integration tests require `.env.integration` with `AWS_SECRET_NAME`, `AWS_REGION`,
`INTEGRATION_REQUESTER_ID`, `INTEGRATION_APPROVER_ID`, `INTEGRATION_APP`, `INTEGRATION_TAG`,
and `CONFIG_PATH`.

## Architecture

deploy-bot is a Go Slack bot that provides approval-gated deployments. The flow is:

1. **Slack → Bot**: Developer runs `/deploy <app>` → bot opens a modal, creates a GitHub PR that updates `newTag:` in a kustomization.yaml in the gitops repo, posts to the deploy channel.
2. **Slack → Bot**: Approver clicks Approve/Reject → bot merges or closes the PR.
3. **GitHub PR → Argo CD**: Merged PRs are picked up by Argo CD which applies the new image tag.

### Key architectural points

**Leader election** (`internal/election`): Uses `coordination.k8s.io/leases`. Only the leader runs the Slack socket mode loop, ECR cache, and sweeper. On leadership loss, `log.Fatal` restarts the pod. The leader context is passed to all leader-only components so they stop cleanly.

**Config vs Secrets split**: `config.json` (mounted as a ConfigMap, hot-reloaded) holds app definitions, GitHub/Slack config, and AWS settings. Secrets (tokens, Redis addr) come from AWS Secrets Manager at startup via `AWS_SECRET_NAME` env var.

**App config**: Each app entry requires an `environment` field (e.g. `"dev"`, `"prod"`). This is included in lock keys, branch names, PR titles, and all user-facing messages so deployments across environments are unambiguous.

**Redis state** (`internal/store`): Pending deploys are stored as `pending:<pr_number>` keys with a TTL equal to `stale_duration`. Per-app deploy locks use `lock:<env>/<app>` keys (SET NX with `lock_ttl` TTL) so the same app in different environments locks independently. The history list (`history`) holds up to 100 `HistoryEntry` records, newest-first, populated by every completion path (approved, rejected, expired, cancelled).

**Identity chain** (`internal/validator`): Slack user ID → email (Slack API) → GitHub login (GitHub API) → team membership check. Used to gate deployer and approver actions.

**Sweeper** (`internal/sweeper`): Polls Redis every 5 minutes for expired pending deploys, closes their PRs, notifies the requester, releases locks, and pushes history entries.

**Slack rate limiting** (`internal/slackclient`): All Slack API calls go through a `Poster` wrapper that retries on 429 responses with configurable max retries and wait duration.

**Config hot-reload** (`internal/config`): `config.Watch` polls the config file mtime every 30s and also responds to SIGHUP. On reload, the ECR cache is updated with any new apps.

### Package map

| Package | Responsibility |
|---|---|
| `cmd/bot` | Wiring: loads config/secrets, constructs all components, runs election loop |
| `cmd/receiver` | Slack Socket Mode receiver: accepts events, validates deploy modal submissions inline, enqueues to Redis Streams |
| `internal/bot` | Slack event loop, slash command handlers, interaction (button) handlers, modal builders |
| `internal/store` | Redis operations: pending deploys, per-app/env locks, history list |
| `internal/sweeper` | Expires stale deploys on a ticker; reconciles GitHub on startup |
| `internal/github` | PR creation, merge, close, commenting |
| `internal/slackclient` | Slack `Poster` interface with rate-limit retry wrapper |
| `internal/validator` | Slack→GitHub identity resolution, team membership gating |
| `internal/ecr` | ECR tag listing, in-memory cache with periodic refresh |
| `internal/audit` | S3 audit log writes |
| `internal/election` | Kubernetes lease-based leader election wrapper |
| `internal/config` | Config struct, file loading, hot-reload watcher, `Holder` (atomic pointer) |
| `internal/metrics` | Prometheus counters/gauges (pending deploys, deploy events by app/outcome) |
| `internal/health` | `/healthz` (liveness) and `/readyz` (readiness — gated on ECR cache populated) endpoints |
| `internal/queue` | Redis Streams producer/consumer for Slack event dispatch |

### Adding a new app

Add an entry to `config.json` under `apps` with at minimum `app`, `environment`, `kustomize_path`, and `ecr_repo`. No code changes needed. The ECR cache will pick it up on the next config reload.

### Branch and commit naming

Branches: `deploy/<env>-<app>-<tag>` (tag sanitized: `/`, `:`, `+`, ` ` → `-`)

Commits and PR titles: `deploy(<env>/<app>): update image tag to <tag>`

### Slash commands

| Command | Handler |
|---|---|
| `/deploy` | Opens modal |
| `/deploy <app>` | Opens modal pre-selected to app |
| `/deploy status` | Lists pending deploys |
| `/deploy history [app]` | Shows last N completed events |
| `/deploy cancel <pr>` | Cancels requester's own deploy |
| `/deploy nudge <pr>` | Re-pings approver |
| `/deploy rollback <app>` | Deploys the previously merged tag |
| `/deploy tags <app>` | Lists recent ECR tags |
| `/deploy tags <app> <tag>` | Validates a specific tag |
