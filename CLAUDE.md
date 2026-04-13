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

# Format Go files
make fmt            # write changes
make fmt-check      # check only (CI-friendly)

# Lint
make lint

# All checks (fmt-check + lint + unit tests)
make check

# Container image (Podman)
make image          # build
make push           # build + push to ECR
make ecr-login      # authenticate Podman to ECR
```

**Before pushing, always run `make check`.** This runs `gofmt` verification, `golangci-lint`, and unit tests. Do not push code that fails any of these.

**Don't open standalone PRs for trivial doc-only changes.** Things like a new `TODO.md` entry, a small note in a comment, or a one-line `CLAUDE.md` clarification should be folded into whatever branch already has related work — or queued in the working tree until the next non-trivial PR. A separate PR per harmless doc note is more review burden than the change is worth.

To create a release, use `make release BUMP=patch|minor|major`. This triggers the GitHub Actions release workflow. Never create tags locally.

Unit tests use `miniredis` -- no real Redis needed.

Integration tests require `.env.integration` with `AWS_SECRET_NAME`,
`INTEGRATION_REQUESTER_ID`, `INTEGRATION_REQUESTER_USERNAME`, `INTEGRATION_APPROVER_ID`,
`INTEGRATION_APP`, and optionally `CONFIG_PATH` (defaults to `testdata/config.json`).

**Critical: stop all other workers before running integration tests.** The test harness starts its own queue worker. If another worker is running — a deploy-bot pod in the k8s cluster, a local `bin/bot` process, or a leftover `go test` — it will race the test worker for Redis stream messages, causing tests to hang or fail intermittently in ways that look like code bugs but aren't. Before running integration tests:

1. Scale down (or verify already scaled down) any deploy-bot worker deployments in the cluster: `kubectl scale deploy deploy-bot-worker -n deploy-bot --replicas=0`
2. Kill any local bot/receiver processes (`pgrep -f 'bin/bot|bin/receiver|go test.*integration'`)
3. If tests are failing with unexplained timeouts or "message not delivered" errors after you've verified the code is correct, suspect a stray worker. Restarting the cluster and local machine is a last resort but has resolved this before.

## Architecture

deploy-bot is a Go Slack bot that provides approval-gated deployments. The flow is:

1. **Slack/ECR -> Bot**: Developer runs `/deploy <app>`, mentions `@bot deploy <app> <tag>`, or an ECR push event fires -> bot creates a GitHub PR that updates `newTag:` in a kustomization.yaml in the gitops repo, posts to the deploy channel.
2. **Slack -> Bot**: Approver clicks Approve/Reject -> bot merges or closes the PR. For `auto_deploy` apps, the bot merges immediately.
3. **GitHub PR -> Argo CD**: Merged PRs are picked up by Argo CD which applies the new image tag.

### Key architectural points

**Config vs Secrets split**: `config.json` (mounted via configMapGenerator, hot-reloaded) holds app definitions, GitHub/Slack config, and AWS settings. Secrets (tokens, Redis addr) come from a JSON file via `SECRETS_PATH` env var or from AWS Secrets Manager via `AWS_SECRET_NAME` env var. Bot and receiver use separate secrets so each component only accesses the credentials it needs. A second `discovered.json` file (optional, from repo scanner) provides repo-sourced app entries that are merged at load time.

**App config**: Each app entry requires an `environment` field (e.g. `"dev"`, `"prod"`). The `app` field is the base name (e.g. `"myapp"`); the bot constructs the composite `app-environment` (e.g. `myapp-dev`, `myapp-prod`) internally via `FullName()`. This composite is used in lock keys, branch names, PR titles, and all user-facing messages so deployments across environments are unambiguous.

**Durable state** (`internal/store`, `internal/store/postgres`): Deploy history and in-flight pending deploys are stored in **Postgres** (required on 3.0+). `history` holds completed deploy events (approved, rejected, expired, cancelled) with no retention cap (retention is governed by a background ticker, default 2 years, 390-day floor). `pending_deploys` holds in-flight deploys with a composite PK `(github_org, github_repo, pr_number)` for multi-repo future-proofing; rows are deleted on state transition to a terminal event (the matching terminal record goes into `history`). Schema is managed by goose migrations embedded in the bot binary; migrations run at startup only when `postgres.auto_migrate` is true, gated by a Postgres advisory lock. See `docs/postgres-setup.md`.

**Redis state** (`internal/store`): Per-app deploy locks use `lock:<env>/<app>` keys (SET NX with `lock_ttl` TTL) so the same app in different environments locks independently. User events and ECR events use separate Redis streams (`user:events` and `ecr:events`); the worker drains the user stream with priority before checking the ECR stream. ArgoCD notifications use a third stream (`argocd:events`). Deploy thread parent timestamps are stored as `thread:<env>` keys with a 10-minute TTL, created atomically (SET NX) to prevent duplicate parent messages from concurrent workers. Redis no longer needs to be durable (no AOF/RDB required) — all durable state is in Postgres.

**Authorization** (`internal/validator`, `internal/approvers`): Users are authorized via OR logic across three configurable sources: direct Slack user IDs, Slack user group membership, and GitHub team membership (Slack user ID -> email -> GitHub login -> team check). The `authorization` config section defines which sources are active. The approvers cache pre-fetches all sources into a single Redis set for fast modal validation; the validator does live checks authoritatively in the worker. For users with private GitHub emails, `identity_overrides` (top-level config field) provides a manual Slack user ID to GitHub login mapping.

**Sweeper** (`internal/sweeper`): Polls Postgres every 5 minutes for expired pending deploys (from `pending_deploys` table), closes their PRs, notifies the requester, releases locks (Redis), and pushes history entries to the `history` table.

**Slack rate limiting** (`internal/slackclient`): All Slack API calls go through a `Poster` wrapper that retries on 429 responses with configurable max retries and wait duration.

**Config hot-reload** (`internal/config`): `config.Watch` polls both the primary config and discovered apps file mtimes every 30s and also responds to SIGHUP. On reload, the ECR cache is updated with any new apps.

**ECR push-triggered deploys** (`internal/ecrpoller`): Controlled by `ecr_auto_deploy` config (with an `enabled` boolean and `sqs_queue_url`). The receiver polls an SQS queue for EventBridge ECR push events, matches them to all apps sharing the ECR repo (not just the first match), validates tags, checks locks, and enqueues `ECRPushEvent`s to the `ecr:events` Redis stream. The worker handler creates PRs and either auto-merges or requests approval based on `auto_deploy` config.

**Repo-sourced app discovery** (`internal/reposcanner`): The receiver scans GitHub repos for `.deploy-bot.json` files, validates entries, detects conflicts with operator config, and writes the discovered apps to a Kubernetes ConfigMap. The bot merges these at load time (operator config always wins on conflicts).

**@mention support** (`internal/bot/mention.go`): The receiver handles `app_mention` events, strips the bot mention prefix, and enqueues `AppMentionEvent`s. The bot dispatches these to the same commands available via slash commands, with channel-visible responses.

### Package map

| Package | Responsibility |
|---|---|
| `cmd/bot` | Wiring: loads config/secrets, constructs all components, runs sweeper and queue worker with distributed Redis locks |
| `cmd/receiver` | Slack Socket Mode receiver: accepts events, validates deploy modal submissions inline, enqueues to Redis Streams. Also runs ECR poller and repo scanner |
| `cmd/deploy-bot-config` | Standalone CLI for validating `.deploy-bot.json` files |
| `cmd/migrate-redis-to-postgres` | One-shot 2.x → 3.0 data migration (copies Redis history + pending to Postgres) |
| `internal/bot` | Slash command handlers, @mention handlers, interaction (button) handlers, modal builders, ECR push handler |
| `internal/store` | Store facade: pending deploys + history (Postgres-backed), per-app/env locks + thread ts (Redis-backed) |
| `internal/store/postgres` | Postgres pool, goose migrations, retention ticker, RDS IAM auth |
| `internal/storetest` | Test helper: shared testcontainer + miniredis for unit tests |
| `internal/sweeper` | Expires stale deploys on a ticker; reconciles GitHub on startup |
| `internal/github` | PR creation, merge, close, commenting, commit statuses |
| `internal/slackclient` | Slack `Poster` interface with rate-limit retry wrapper |
| `internal/validator` | Slack->GitHub identity resolution, team membership gating |
| `internal/ecr` | ECR tag listing, in-memory cache with periodic refresh |
| `internal/ecrpoller` | SQS long-poll loop for ECR push events from EventBridge |
| `internal/repoconfig` | Shared `.deploy-bot.json` schema, parsing (with apiVersion), and validation (stdlib-only) |
| `internal/reposcanner` | Repo-sourced app discovery: scan, validate, conflict detect, ConfigMap write |
| `internal/sanitize` | Input sanitization for user-provided text in Slack/GitHub and tag/branch name validation |
| `internal/approvers` | Cached team membership lookups with periodic refresh |
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
| `/deploy list` | Lists pending deploys (alias: `status`) |
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
