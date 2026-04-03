# deploy-bot

> This project was largely built with [Claude Code](https://claude.ai/code). Human contributors are welcome.

A Slack bot that gates Kubernetes deployments behind an approval workflow. Developers request deployments via `/deploy` or `@bot deploy`, approvers approve or reject via Slack buttons, and the bot creates and merges GitHub PRs that update kustomize image tags in a GitOps repo. Argo CD picks up merged PRs and deploys.

## How it works

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
    |  (SQS)           |-- enqueue ------->|               |                     |
    |                  |                   |-- event ----->|                     |
    |                  |                   |               |-- create PR ------->|
    |                  |                   |               |   (auto-merge or    |
    |                  |                   |               |    request approval)|
```

Two processes share a single container image:

- **receiver** -- connects to Slack via Socket Mode, validates incoming events, and enqueues them to a Redis Stream. Also polls SQS for ECR push events and scans repos for app config (when enabled). Stateless; run 2+ replicas.
- **worker** -- consumes events from the stream, runs all business logic (GitHub API, ECR, audit logging). Run 2+ replicas; Redis Streams consumer groups ensure each event is processed once.

## Prerequisites

- Kubernetes cluster (EKS recommended)
- AWS -- Secrets Manager, ECR (for app images), S3 (audit log, optional), SQS + EventBridge (ECR push deploys, optional)
- ElastiCache for Redis -- Multi-AZ with automatic failover and AOF persistence enabled. **Redis is required; the bot will not start without it.** See [docs/redis-resilience.md](docs/redis-resilience.md) for behaviour during outages and after a flush.
- GitHub fine-grained PAT with repository (contents, pull requests, commit statuses) and organisation (members read) permissions
- Slack App in Socket Mode with the following bot scopes:
  `app_mentions:read`, `commands`, `chat:write`, `users:read`, `users:read.email`, `im:write`

## Configuration

See [docs/configuration.md](docs/configuration.md) for the full configuration reference covering secrets, config file fields, ECR events, repo discovery, and per-app settings.

Quick start: copy `deploy/configmap.yaml`, fill in your values, and apply.

## Deployment

### AWS resources

Use the Terraform module in `terraform/` to create the IAM role, policies, and optionally the SQS queue and EventBridge rule for ECR push events:

```hcl
module "deploy_bot" {
  source = "./terraform"

  region                = "us-east-1"
  account_id            = "123456789012"
  eks_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
  eks_oidc_provider_url = "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
  audit_bucket          = "my-audit-logs"
  ecr_events_enabled    = true
}
```

The module grants ECR read access to **all repositories** in the account so new apps work without IAM changes. The EventBridge rule captures all ECR push events account-wide; the bot filters by configured apps and tag patterns. See [terraform/README.md](terraform/README.md) for details.

### Slack app setup

Use the `slack-manifest.json` file at the root of this repository to create the app in one step:

1. Go to [api.slack.com/apps](https://api.slack.com/apps) and click **Create New App > From a manifest**. Select your workspace, paste the contents of `slack-manifest.json`, and click through to create the app.

2. Go to **Socket Mode** in the sidebar. Click **Generate Token**, name it (e.g. `socket`), add the `connections:write` scope, and click **Generate**. Copy the token (starts with `xapp-`) -- this is your `slack_app_token`.

3. Go to **OAuth & Permissions** and click **Install to Workspace**. Approve the permissions. Copy the **Bot User OAuth Token** (starts with `xoxb-`) -- this is your `slack_bot_token`.

### GitHub permissions

A **fine-grained Personal Access Token** is required. Store it in the `github_token` secret field.

Create it at GitHub > Settings > Developer settings > Personal access tokens > Fine-grained tokens. Set the resource owner to your organisation.

**Repository permissions** -- scope to the gitops repo only:

| Permission | Level | Why |
|---|---|---|
| Contents | Read & write | Push kustomization branches |
| Pull requests | Read & write | Create, merge, close PRs and post comments |
| Commit statuses | Read & write | Set config validation status on repos (repo discovery) |

**Organisation permissions:**

| Permission | Level | Why |
|---|---|---|
| Members | Read | Check deployer/approver team membership |

### Apply Kubernetes manifests

The `deploy/` directory is a Kustomize base:

```bash
# Review and customise
kubectl kustomize deploy/

# Apply
kubectl apply -k deploy/
```

Or apply directly:

```bash
kubectl create namespace deploy-bot
kubectl apply -f deploy/
```

Update the image tag in `deploy/deployment.yaml` to match the version you want to run. The public image is:

```
ghcr.io/ezubriski/deploy-bot:<tag>
```

The `deploy-bot-discovered` ConfigMap is created automatically by the repo scanner and should **not** be version-controlled. It is marked `optional: true` in the deployment so the bot starts without it.

### Endpoints

| Process | Port | Paths |
|---|---|---|
| worker | 9090 | `/healthz` (liveness), `/readyz` (readiness), `/metrics` (Prometheus) |
| receiver | 8080 | `/healthz` (liveness) |

## Commands

App names include the environment suffix (e.g. `myapp-dev`, `myapp-prod`). Use `apps` to list configured apps.

### Slash commands

| Command | Description |
|---|---|
| `/deploy` | Open the deployment request modal |
| `/deploy <app-env>` | Open the modal pre-selected to an app |
| `/deploy status` | List all pending deployments |
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
| `@bot status` | List pending deployments (visible in channel) |
| `@bot history [app-env]` | Show recent deploys |
| `@bot apps` | List configured apps |
| `@bot conflicts` | List config conflicts |
| `@bot tags <app-env>` | List recent tags |
| `@bot cancel <pr>` | Cancel your own deployment |
| `@bot nudge <pr>` | Remind the approver |
| `@bot rollback <app-env>` | Deploy the previous tag |
| `@bot help` | Show command help |

Mention responses are posted in-channel (threaded if the mention was in a thread). The slash command provides a guided modal with dropdowns and validation.

## Development

```bash
make build              # build both binaries to ./bin
make test               # run unit tests
make test-pkg PKG=./internal/store/...  # single package
make test-integ         # integration tests (requires .env.integration)
make test-integ-single RUN=TestDeployAndApprove  # single integration test
make lint               # run golangci-lint
make image              # build container image with Podman (git short SHA)
make ecr-login          # authenticate Podman to ECR
make push               # build and push to ECR
make clean              # remove ./bin
```

Override the image tag: `make push TAG=v1.2.3`

Integration tests require a `.env.integration` file with `AWS_SECRET_NAME`, `AWS_REGION`, `INTEGRATION_REQUESTER_ID`, `INTEGRATION_REQUESTER_USERNAME`, `INTEGRATION_APPROVER_ID`, `INTEGRATION_APP`, `INTEGRATION_TAG`, and `CONFIG_PATH`.

## Monitoring

Worker pods expose Prometheus metrics on port `9090` at `/metrics` and are annotated for auto-discovery:

```yaml
prometheus.io/scrape: "true"
prometheus.io/port:   "9090"
prometheus.io/path:   "/metrics"
```

If you are running the Prometheus Operator, use a `ServiceMonitor`:

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

## CI

GitHub Actions runs on pull requests and pushes to `main` and version tags (`v*`):

1. **Test** -- runs `make test` (unit tests only; no external dependencies).
2. **Build** (push events only, only if tests pass) -- builds with Podman and pushes to ghcr.io, tagged with the short SHA (`main` pushes) or the version (`v*` tags), plus `latest`. Also pushes to ECR if the `ECR_REGISTRY` repository secret is set.

## Further reading

- [Configuration reference](docs/configuration.md)
- [ECR push-triggered deploys](docs/ecr-push-triggered-deploys.md)
- [Repo-sourced app discovery](docs/repo-sourced-app-discovery.md)
- [No-op deploy handling](docs/no-op-deploy-handling.md)
- [Merge conflict handling](docs/merge-conflict-handling.md)
- [Redis resilience](docs/redis-resilience.md)
- [Integration test setup](docs/integration-test-setup.md)
- [Terraform module](terraform/README.md)
