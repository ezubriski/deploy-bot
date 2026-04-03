# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

Use `make help` to list all targets. Common ones:

```bash
# Build all binaries (bot, receiver, deploy-bot-config) to ./bin
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

Unit tests use `miniredis` -- no real Redis needed.

Integration tests require `.env.integration` with `AWS_SECRET_NAME`,
`INTEGRATION_REQUESTER_ID`, `INTEGRATION_REQUESTER_USERNAME`, `INTEGRATION_APPROVER_ID`,
`INTEGRATION_APP`, and optionally `CONFIG_PATH` (defaults to `testdata/config.json`).

## Architecture

deploy-bot is a Go Slack bot that provides approval-gated deployments. The flow is:

1. **Slack/ECR -> Bot**: Developer runs `/deploy <app>`, mentions `@bot deploy <app> <tag>`, or an ECR push event fires -> bot creates a GitHub PR that updates `newTag:` in a kustomization.yaml in the gitops repo, posts to the deploy channel.
2. **Slack -> Bot**: Approver clicks Approve/Reject -> bot merges or closes the PR. For `auto_deploy` apps, the bot merges immediately.
3. **GitHub PR -> Argo CD**: Merged PRs are picked up by Argo CD which applies the new image tag.

### Key architectural points

**Config vs Secrets split**: `config.json` (mounted via configMapGenerator, hot-reloaded) holds app definitions, GitHub/Slack config, and AWS settings. Secrets (tokens, Redis addr) come from a JSON file via `SECRETS_PATH` env var or from AWS Secrets Manager via `AWS_SECRET_NAME` env var. Bot and receiver use separate secrets so each component only accesses the credentials it needs. A second `discovered.json` file (optional, from repo scanner) provides repo-sourced app entries that are merged at load time.

**App config**: Each app entry requires an `environment` field (e.g. `"dev"`, `"prod"`). App names include the environment (e.g. `myapp-dev`, `myapp-prod`). This is included in lock keys, branch names, PR titles, and all user-facing messages so deployments across environments are unambiguous.

**Redis state** (`internal/store`): Pending deploys are stored as `pending:<pr_number>` keys with a TTL equal to `stale_duration`. Per-app deploy locks use `lock:<env>/<app>` keys (SET NX with `lock_ttl` TTL) so the same app in different environments locks independently. The history list (`history`) holds up to 100 `HistoryEntry` records, newest-first, populated by every completion path (approved, rejected, expired, cancelled).

**Identity chain** (`internal/validator`): Slack user ID -> email (Slack API) -> GitHub login (GitHub API) -> team membership check. Used to gate deployer and approver actions.

**Sweeper** (`internal/sweeper`): Polls Redis every 5 minutes for expired pending deploys, closes their PRs, notifies the requester, releases locks, and pushes history entries.

**Slack rate limiting** (`internal/slackclient`): All Slack API calls go through a `Poster` wrapper that retries on 429 responses with configurable max retries and wait duration.

**Config hot-reload** (`internal/config`): `config.Watch` polls both the primary config and discovered apps file mtimes every 30s and also responds to SIGHUP. On reload, the ECR cache is updated with any new apps.

**ECR push-triggered deploys** (`internal/ecrpoller`): The receiver polls an SQS queue for EventBridge ECR push events, matches them to configured apps, validates tags, checks locks, and enqueues `ECRPushEvent`s to the Redis stream. The worker handler creates PRs and either auto-merges or requests approval based on `auto_deploy` config.

**Repo-sourced app discovery** (`internal/reposcanner`): The receiver scans GitHub repos for `.deploy-bot.json` files, validates entries, detects conflicts with operator config, and writes the discovered apps to a Kubernetes ConfigMap. The bot merges these at load time (operator config always wins on conflicts).

**@mention support** (`internal/bot/mention.go`): The receiver handles `app_mention` events, strips the bot mention prefix, and enqueues `AppMentionEvent`s. The bot dispatches these to the same commands available via slash commands, with channel-visible responses.

### Package map

| Package | Responsibility |
|---|---|
| `cmd/bot` | Wiring: loads config/secrets, constructs all components, runs sweeper and queue worker with distributed Redis locks |
| `cmd/receiver` | Slack Socket Mode receiver: accepts events, validates deploy modal submissions inline, enqueues to Redis Streams. Also runs ECR poller and repo scanner |
| `cmd/deploy-bot-config` | Standalone CLI for validating `.deploy-bot.json` files |
| `internal/bot` | Slash command handlers, @mention handlers, interaction (button) handlers, modal builders, ECR push handler |
| `internal/store` | Redis operations: pending deploys, per-app/env locks, history list |
| `internal/sweeper` | Expires stale deploys on a ticker; reconciles GitHub on startup |
| `internal/github` | PR creation, merge, close, commenting, commit statuses |
| `internal/slackclient` | Slack `Poster` interface with rate-limit retry wrapper |
| `internal/validator` | Slack->GitHub identity resolution, team membership gating |
| `internal/ecr` | ECR tag listing, in-memory cache with periodic refresh |
| `internal/ecrpoller` | SQS long-poll loop for ECR push events from EventBridge |
| `internal/repoconfig` | Shared `.deploy-bot.json` schema, parsing (with apiVersion), and validation (stdlib-only) |
| `internal/reposcanner` | Repo-sourced app discovery: scan, validate, conflict detect, ConfigMap write |
| `internal/sanitize` | Input sanitization for user-provided text in Slack/GitHub and tag/branch name validation |
| `internal/approvers` | Cached approver team membership lookups with periodic refresh |
| `internal/audit` | Audit log writes (S3 or zap fallback) |
| `internal/config` | Config struct, file loading, discovered-path merge, hot-reload watcher, `Holder` |
| `internal/metrics` | Prometheus counters/gauges (pending deploys, deploy events by app/outcome) |
| `internal/health` | `/healthz` (liveness) and `/readyz` (readiness -- gated on ECR cache populated) endpoints |
| `internal/queue` | Redis Streams producer/consumer; event types: slash command, interaction, ECR push, app mention |
| `internal/buffer` | In-memory retry buffer for Redis backpressure |

### Adding a new app

**Operator-managed**: Add an entry to `config.json` under `apps` with at minimum `app`, `environment`, `kustomize_path`, and `ecr_repo`. No code changes needed. The ECR cache will pick it up on the next config reload.

**Repo-sourced**: Place a `.deploy-bot.json` in the repo root (see [docs/configuration.md](docs/configuration.md)). The scanner picks it up on the next poll cycle.

### Branch and commit naming

Branches: `deploy/<env>-<app>-<tag>` (tags with unsafe characters are rejected by `sanitize.TagIsSafe`; the branch suffix is sanitized by `sanitize.BranchName` which replaces `/`, `:`, `+`, ` `, `~`, `^`, `*`, `?`, `[`, `]`, `\`, `..` with `-`, collapses runs, and trims)

Commits and PR titles: `deploy(<env>/<app>): update image tag to <tag>`

### Slash commands

| Command | Handler |
|---|---|
| `/deploy` | Opens modal |
| `/deploy <app-env>` | Opens modal pre-selected to app |
| `/deploy status` | Lists pending deploys |
| `/deploy history [app-env]` | Shows last N completed events |
| `/deploy apps` | Lists configured apps with source (operator/repo) |
| `/deploy conflicts` | Lists repo-sourced apps blocked by operator config |
| `/deploy cancel <pr>` | Cancels requester's own deploy |
| `/deploy nudge <pr>` | Re-pings approver |
| `/deploy rollback <app-env>` | Deploys the previously merged tag |
| `/deploy tags <app-env>` | Lists recent ECR tags |
| `/deploy tags <app-env> <tag>` | Validates a specific tag |
| `/deploy help` | Shows command help |

### @mention commands

All commands are also available via `@bot <command>` in any channel. Responses are posted in-channel (threaded if in a thread). `deploy` accepts positional args: `@bot deploy <app-env> <tag> [@approver] [reason]`.

### Deployment

Kubernetes manifests are in `deploy/` as a Kustomize base (`receiver.yaml`, `worker.yaml`, `rbac.yaml`, `service.yaml`, `namespace.yaml`, `redis.yaml`). Config is mounted via `configMapGenerator` with suffix hashing disabled. AWS resources (IAM roles, policies, SQS, EventBridge) are in `terraform/`. See [docs/configuration.md](docs/configuration.md) for the full config reference.
