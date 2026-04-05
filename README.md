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

Setup involves collecting credentials from multiple services. Copy the env template to track your progress:

```bash
cp setup.env.example setup.env
```

Fill in each variable as you complete the steps below. Source the file before running any commands:

```bash
source setup.env
```

### 1. Create the Slack app

Use the `slack-manifest.json` file at the root of this repository:

1. Go to [api.slack.com/apps](https://api.slack.com/apps) and click **Create New App > From a manifest**. Select your workspace, paste the contents of `slack-manifest.json`, and create the app.
2. Go to **Socket Mode** in the sidebar. Click **Generate Token**, name it (e.g. `socket`), add the `connections:write` scope, and generate. Copy the token (starts with `xapp-`) -- this is your `slack_app_token`.
3. Go to **OAuth & Permissions** and click **Install to Workspace**. Copy the **Bot User OAuth Token** (starts with `xoxb-`) -- this is your `slack_bot_token`.

### 2. Create GitHub PATs

Fine-grained PATs cannot mix permission levels across repositories. You need one token for the gitops repo (read/write) and, if using repo-sourced app discovery, a second read-only token for discoverable repos.

Set these shell variables first — they're used in the creation URLs and validation script below:

```bash
export GITHUB_ORG=your-org
export GITHUB_REPO=your-gitops-repo
```

**Primary token** (`github_token`) — [create here](https://github.com/settings/personal-access-tokens/new), scoped to the gitops repo only:

| Scope | Setting |
|---|---|
| Resource owner | `$GITHUB_ORG` |
| Repository access | Only select repositories → `$GITHUB_REPO` |

Repository permissions:

| Permission | Level | Why |
|---|---|---|
| Contents | Read & write | Push kustomization branches |
| Pull requests | Read & write | Create, merge, close PRs and post comments |
| Issues | Read & write | PR comments, labels |
| Commit statuses | Read & write | Set commit statuses |
| Metadata | Read | Repository info (granted automatically) |

Organization permissions:

| Permission | Level | Why |
|---|---|---|
| Members | Read | Check deployer/approver team membership |

**Scanner token** (`github_scanner_token`, optional) — [create here](https://github.com/settings/personal-access-tokens/new), scoped to all repos (or repos with your discovery prefix):

| Scope | Setting |
|---|---|
| Resource owner | `$GITHUB_ORG` |
| Repository access | All repositories (or select repos with your prefix) |

Repository permissions:

| Permission | Level | Why |
|---|---|---|
| Contents | Read | Read `.deploy-bot.json` from repos |
| Commit statuses | Read & write | Set config validation status on discovered repos |
| Metadata | Read | Repository info (granted automatically) |

If `github_scanner_token` is not set, the primary `github_token` is used for scanning. This works if your primary token has access to all repos, but means the gitops write permissions are shared with the scanner.

**Validate your tokens:**

```bash
export DEPLOY_BOT_TOKEN=github_pat_...
export DEPLOY_BOT_SCANNER_TOKEN=github_pat_...  # optional
./scripts/validate-token.sh
```

The script checks every required permission and reports what's missing:

```
Validating primary token (DEPLOY_BOT_TOKEN)
─────────────────────────────────────────────
  ✓ Token is valid
  ✓ Can access your-org/gitops
  ✓ Contents: read (git refs)
  ✓ Contents: write (can create branches)
  ✓ Pull requests: read
  ✓ Pull requests: write (can create PRs)
  ✓ Issues: write (can manage labels)
  ✓ Organization members: read (team listing)

All checks passed.
```

### 3. Set up AWS resources

Set these shell variables — they're used in the Terraform and AWS CLI commands below:

```bash
export AWS_REGION=us-east-1
export AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
export DEPLOY_BOT_SECRET_NAME=deploy-bot/secrets
export AUDIT_BUCKET=my-audit-logs           # S3 bucket for audit logs (optional)
```

**Option A: Terraform (recommended)**

```hcl
module "deploy_bot" {
  source = "github.com/ezubriski/deploy-bot//terraform"

  region     = "us-east-1"
  account_id = "123456789012"

  # IRSA (EKS) — omit both to skip role creation
  eks_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
  eks_oidc_provider_url = "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"

  # IAM users instead of roles (non-EKS clusters)
  # identity_type = "user"

  # EC2 trust (self-managed k8s on EC2)
  # enable_ec2_trust = true

  audit_bucket       = "my-audit-logs"  # omit to disable S3 audit logging
  ecr_events_enabled = true             # set false to skip SQS/EventBridge
}
```

The module creates:
- Separate IAM policies for bot (worker) and receiver (least-privilege)
- IAM roles (IRSA or EC2 trust) or IAM users depending on `identity_type`
- SQS queue + EventBridge rule for ECR push events (when `ecr_events_enabled = true`)

See [terraform/README.md](terraform/README.md) for the full variable and output reference.

**Option B: AWS CLI (manual)**

Create the Secrets Manager secret, S3 audit bucket, and (optionally) the ECR events pipeline:

```bash
# Secrets Manager secret
aws secretsmanager create-secret \
  --name "${DEPLOY_BOT_SECRET_NAME}" \
  --region "${AWS_REGION}" \
  --secret-string '{}'

# S3 audit bucket (optional)
aws s3 mb "s3://${AUDIT_BUCKET}" --region "${AWS_REGION}"

# SQS queue for ECR push events (optional)
aws sqs create-queue \
  --queue-name deploy-bot-ecr-events \
  --region "${AWS_REGION}" \
  --attributes VisibilityTimeout=300,MessageRetentionPeriod=86400

# EventBridge rule to capture ECR pushes (optional)
aws events put-rule \
  --name deploy-bot-ecr-push \
  --region "${AWS_REGION}" \
  --event-pattern '{
    "source": ["aws.ecr"],
    "detail-type": ["ECR Image Action"],
    "detail": {"action-type": ["PUSH"], "result": ["SUCCESS"]}
  }'

# Wire EventBridge -> SQS (replace <SQS_ARN> with your queue ARN)
aws events put-targets \
  --rule deploy-bot-ecr-push \
  --region "${AWS_REGION}" \
  --targets "Id=deploy-bot-sqs,Arn=<SQS_ARN>"
```

### 4. Store secrets

Populate the Secrets Manager secret with your tokens. The bot and receiver share a single secret:

```bash
aws secretsmanager put-secret-value \
  --secret-id "${DEPLOY_BOT_SECRET_NAME}" \
  --region "${AWS_REGION}" \
  --secret-string "$(cat <<SECRETS
{
  "slack_bot_token": "${SLACK_BOT_TOKEN}",
  "slack_app_token": "${SLACK_APP_TOKEN}",
  "github_token": "${DEPLOY_BOT_TOKEN}",
  "github_scanner_token": "${DEPLOY_BOT_SCANNER_TOKEN:-}",
  "redis_addr": "${REDIS_ADDR}"
}
SECRETS
)"
```

Set the required shell variables before running:

```bash
export SLACK_BOT_TOKEN=xoxb-...     # from step 1
export SLACK_APP_TOKEN=xapp-...     # from step 1
export DEPLOY_BOT_TOKEN=github_pat_...          # from step 2
export DEPLOY_BOT_SCANNER_TOKEN=github_pat_...  # from step 2 (optional)
export REDIS_ADDR=your-redis:6379               # your Redis endpoint
```

Set the `AWS_SECRET_NAME` and `AWS_REGION` environment variables on both Kubernetes deployments to point to the secret.

### 5. Customize config.json

Copy `deploy/config.json` and fill in your values:

```json
{
  "github_owner": "your-org",
  "github_repo": "your-gitops-repo",
  "slack_channel_id": "C0123456789",
  "deployer_team": "your-org/developers",
  "approver_team": "your-org/platform",
  "apps": [
    {
      "app": "myapp",
      "environment": "dev",
      "kustomize_path": "apps/myapp/overlays/dev",
      "ecr_repo": "myapp"
    }
  ]
}
```

See [docs/configuration.md](docs/configuration.md) for the full reference.

### 6. Deploy with Kustomize

The `deploy/` directory is a Kustomize base. Create an overlay for your cluster:

```yaml
# overlay/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  - github.com/ezubriski/deploy-bot/deploy

images:
  - name: ghcr.io/ezubriski/deploy-bot
    newTag: latest

configMapGenerator:
  - name: deploy-bot-config
    files:
      - config.json
    behavior: replace

generatorOptions:
  disableNameSuffixHash: true
```

Apply:

```bash
kustomize build . | kubectl apply -f -
```

**Config hot-reload:** The base manifests mount ConfigMaps as directories (not `subPath`), so Kubernetes automatically updates the mounted files when the ConfigMap changes. The bot watches for file changes every 30 seconds (or on SIGHUP) and reloads without restarting. If you use `subPath` mounts in your overlay, Kubernetes will **not** update the files — avoid `subPath` for config volumes.

### 7. Test it

Run `/deploy help` in Slack. You should see the command help. Run `/deploy apps` to verify your app config loaded.

### 8. Add your first app

Add an entry to `config.json` with `app`, `environment`, `kustomize_path`, and `ecr_repo`. The bot picks it up within 30 seconds -- no restart needed.

### 9. (Optional) Enable ECR push deploys

1. Set `ecr_events_enabled = true` in the Terraform module (creates the SQS queue and EventBridge rule).
2. Add `ecr_events.sqs_queue_url` to your config.
3. Set `auto_deploy: true` on apps that should deploy without approval.
4. Push an image to ECR and watch it deploy.

See [docs/ecr-push-triggered-deploys.md](docs/ecr-push-triggered-deploys.md) for the full guide.

### 10. (Optional) Enable repo-sourced app discovery

1. Enable the repo scanner in config (`repo_discovery.enabled: true`).
2. App teams create a `.deploy-bot.json` in their repo root.
3. Use the `deploy-bot-config` CLI or GitHub Action to validate in CI.
4. The bot discovers new apps on the next scan cycle.

See [docs/repo-sourced-app-discovery.md](docs/repo-sourced-app-discovery.md) for the full guide.

### 11. (Recommended) Enforce naming conventions

When multiple teams manage their own apps, enable `enforce_repo_naming` to derive app names and kustomize paths from repository names. This prevents teams from accidentally targeting each other's directories or choosing conflicting names — the convention makes it structurally impossible.

```json
{
  "repo_discovery": {
    "enabled": true,
    "enforce_repo_naming": true,
    "kustomize_path_template": "{env}/{repo}/kustomization.yaml",
    "default_tag_pattern": "^v[0-9]+\\.[0-9]+\\.[0-9]+$"
  }
}
```

With this enabled, a team's `.deploy-bot.json` only needs to specify what's unique to their app — environment and ECR repo:

```json
{
  "apiVersion": "deploy-bot/v2",
  "apps": [
    {"environment": "dev", "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-service"},
    {"environment": "prod", "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-service"}
  ]
}
```

Everything else — app name, kustomize path, tag pattern — is derived from the repo name and operator defaults. Repos that need to deviate from the convention can be added to `exempt_repos`.

See [docs/naming-conventions.md](docs/naming-conventions.md) for the full guide, including configurable path templates, default tag patterns, exemptions, and all conflict scenarios with resolution steps.

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

The IRSA variables (`eks_oidc_provider_arn`, `eks_oidc_provider_url`) are optional. Omit them to create policies without roles.

See [terraform/README.md](terraform/README.md) for the full variable and output reference.

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
  ✓ github.deployer_team
  ✓ github.approver_team
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

- [Configuration reference](docs/configuration.md)
- [ECR push-triggered deploys](docs/ecr-push-triggered-deploys.md)
- [Repo-sourced app discovery](docs/repo-sourced-app-discovery.md)
- [Naming conventions and conflict resolution](docs/naming-conventions.md)
- [No-op deploy handling](docs/no-op-deploy-handling.md)
- [Merge conflict handling](docs/merge-conflict-handling.md)
- [Redis resilience](docs/redis-resilience.md)
- [Integration test setup](docs/integration-test-setup.md)
- [Terraform module](terraform/README.md)
