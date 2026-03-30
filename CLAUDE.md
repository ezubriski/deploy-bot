# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build
go build ./...

# Test all packages
go test ./...

# Test a single package
go test ./internal/store/...

# Run a single test
go test ./internal/store/... -run TestPushHistory_OrderAndTrim

# Lint (if golangci-lint is available)
golangci-lint run
```

Tests use `miniredis` for Redis — no real Redis needed.

## Architecture

deploy-bot is a Go Slack bot that provides approval-gated deployments. The flow is:

1. **Slack → Bot**: Developer runs `/deploy <app>` → bot opens a modal, creates a GitHub PR that updates `newTag:` in a kustomization.yaml in the gitops repo, posts to the deploy channel.
2. **Slack → Bot**: Approver clicks Approve/Reject → bot merges or closes the PR.
3. **GitHub PR → Argo CD**: Merged PRs are picked up by Argo CD which applies the new image tag.

### Key architectural points

**Leader election** (`internal/election`): Uses `coordination.k8s.io/leases`. Only the leader runs the Slack socket mode loop, ECR cache, and sweeper. On leadership loss, `log.Fatal` restarts the pod. The leader context is passed to all leader-only components so they stop cleanly.

**Config vs Secrets split**: `config.json` (mounted as a ConfigMap, hot-reloaded) holds app definitions, GitHub/Slack config, and AWS role ARNs. Secrets (tokens, Redis addr) come from AWS Secrets Manager at startup via `AWS_SECRET_NAME` env var.

**Redis state** (`internal/store`): Pending deploys are stored as `pending:<pr_number>` keys with a TTL equal to `stale_duration`. Per-app deploy locks use `lock:<app>` keys (SET NX with `lock_ttl` TTL). The history list (`history`) holds up to 100 `HistoryEntry` records, newest-first, populated by every completion path (approved, rejected, expired, cancelled).

**Identity chain** (`internal/validator`): Slack user ID → email (Slack API) → GitHub login (GitHub API) → team membership check. Used to gate deployer and approver actions.

**Sweeper** (`internal/sweeper`): Polls Redis every 5 minutes for expired pending deploys, closes their PRs, notifies the requester, releases locks, and pushes history entries.

**Config hot-reload** (`internal/config`): `config.Watch` polls the config file mtime every 30s and also responds to SIGHUP. On reload, the ECR cache is updated with any new apps.

### Package map

| Package | Responsibility |
|---|---|
| `cmd/bot` | Wiring: loads config/secrets, constructs all components, runs election loop |
| `internal/bot` | Slack event loop, slash command handlers, interaction (button) handlers, modal builders |
| `internal/store` | Redis operations: pending deploys, per-app locks, history list |
| `internal/sweeper` | Expires stale deploys on a ticker |
| `internal/github` | PR creation, merge, close, commenting |
| `internal/validator` | Slack→GitHub identity resolution, team membership gating |
| `internal/ecr` | ECR tag listing with STS AssumeRole, in-memory cache with periodic refresh |
| `internal/audit` | S3 audit log writes with STS AssumeRole |
| `internal/election` | Kubernetes lease-based leader election wrapper |
| `internal/config` | Config struct, file loading, hot-reload watcher, `Holder` (atomic pointer) |
| `internal/metrics` | Prometheus counters/gauges (pending deploys, deploy events by app/outcome) |
| `internal/health` | `/healthz` (liveness) and `/readyz` (readiness — gated on ECR cache populated) endpoints |

### Adding a new app

Add an entry to `config.json` under `apps`. No code changes needed. The ECR cache will pick it up on the next config reload.

### Slash commands

| Command | Handler |
|---|---|
| `/deploy` | Opens modal |
| `/deploy <app>` | Opens modal pre-selected to app |
| `/deploy status` | Lists pending deploys |
| `/deploy history [app]` | Shows last N completed events |
| `/deploy cancel <pr>` | Cancels requester's own deploy |
| `/deploy nudge <pr>` | Re-pings approvers |
| `/deploy rollback <app>` | Deploys the previously merged tag |
