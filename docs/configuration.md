# Configuration Reference

The bot uses two configuration sources:

- **`config.json`** — mounted as a ConfigMap, hot-reloaded on file change (30s poll) or SIGHUP. The base Kustomize manifests mount ConfigMaps as directories (not `subPath`) so Kubernetes updates files in place without requiring a pod restart. Avoid `subPath` in overlays or hot-reload will not work.
- **`discovered.json`** — written by the repo scanner (optional), merged at load time

## Secrets

The bot and receiver use **separate secrets** so that each component only has access to the credentials it needs. Secrets are loaded from one of two sources, checked in order:

1. **File** (`SECRETS_PATH` env var) — a JSON file, typically mounted from a Kubernetes Secret
2. **AWS Secrets Manager** (`AWS_SECRET_NAME` env var) — fetched at startup via the AWS SDK

Set exactly one per component. If both are set, `SECRETS_PATH` takes precedence.

### Bot (worker) secret

| Field | Required | Description |
|---|---|---|
| `slack_bot_token` | Yes | Bot User OAuth Token (`xoxb-`) |
| `github_token` | Yes | Fine-grained PAT scoped to the gitops repo — Contents and Pull requests (read/write), org Members (read) |
| `redis_addr` | Yes | Redis endpoint with port (e.g. `redis:6379`) |
| `redis_token` | No | Redis auth token. Mutually exclusive with `redis_iam_auth` |
| `redis_iam_auth` | No | Enable ElastiCache IAM authentication. Requires `redis_user_id` and `redis_replication_group_id`. When true, TLS is enabled automatically |
| `redis_user_id` | When IAM auth | ElastiCache user ID for IAM authentication |
| `redis_replication_group_id` | When IAM auth | ElastiCache replication group ID (used to generate presigned auth tokens) |

The bot does not need `slack_app_token` (Socket Mode is receiver-only) or `github_scanner_token` (repo scanning is receiver-only).

### Receiver secret

| Field | Required | Description |
|---|---|---|
| `slack_bot_token` | Yes | Bot User OAuth Token (`xoxb-`) |
| `slack_app_token` | Yes | App-Level Token (`xapp-`) with `connections:write` scope — required for Socket Mode |
| `github_token` | Yes | Fine-grained PAT — org Members (read) for approver cache |
| `github_scanner_token` | No | Fine-grained PAT scoped to all repos (or discoverable repos) with Contents (read) and Commit statuses (read/write). Used by the repo scanner. Falls back to `github_token` if not set |
| `redis_addr` | Yes | Redis endpoint with port |
| `redis_token` | No | Redis auth token. Mutually exclusive with `redis_iam_auth` |
| `redis_iam_auth` | No | Enable ElastiCache IAM authentication. Requires `redis_user_id` and `redis_replication_group_id`. When true, TLS is enabled automatically |
| `redis_user_id` | When IAM auth | ElastiCache user ID for IAM authentication |
| `redis_replication_group_id` | When IAM auth | ElastiCache replication group ID (used to generate presigned auth tokens) |

### Option A: Kubernetes Secrets (file mount)

```bash
# Bot secret
kubectl create secret generic deploy-bot-worker-secrets \
  --namespace=deploy-bot \
  --from-literal=secrets.json='{
    "slack_bot_token": "xoxb-...",
    "github_token":    "github_pat_...",
    "redis_addr":      "redis:6379"
  }'

# Receiver secret
kubectl create secret generic deploy-bot-receiver-secrets \
  --namespace=deploy-bot \
  --from-literal=secrets.json='{
    "slack_bot_token":      "xoxb-...",
    "slack_app_token":      "xapp-...",
    "github_token":         "github_pat_...",
    "github_scanner_token": "github_pat_...",
    "redis_addr":           "redis:6379"
  }'
```

Set `SECRETS_PATH=/etc/deploy-bot/secrets/secrets.json` in each deployment and mount the respective secret as a volume.

### Option B: AWS Secrets Manager

Create separate secrets for each component:

```bash
# Bot secret (password auth)
aws secretsmanager create-secret \
  --name deploy-bot/bot-secrets \
  --secret-string '{
    "slack_bot_token": "xoxb-...",
    "github_token":    "github_pat_...",
    "redis_addr":      "deploy-bot.xxxxxx.ng.0001.use1.cache.amazonaws.com:6379",
    "redis_token":     "your-elasticache-auth-token"
  }'

# Receiver secret (password auth)
aws secretsmanager create-secret \
  --name deploy-bot/receiver-secrets \
  --secret-string '{
    "slack_bot_token":      "xoxb-...",
    "slack_app_token":      "xapp-...",
    "github_token":         "github_pat_...",
    "github_scanner_token": "github_pat_...",
    "redis_addr":           "deploy-bot.xxxxxx.ng.0001.use1.cache.amazonaws.com:6379",
    "redis_token":          "your-elasticache-auth-token"
  }'
```

The Terraform module scopes each role's Secrets Manager read permission to its own secret (`bot_secrets_manager_secret_name` and `receiver_secrets_manager_secret_name`), preventing cross-access.

### Option C: AWS Secrets Manager with ElastiCache IAM auth

When using ElastiCache with IAM authentication, replace `redis_token` with the IAM auth fields. TLS is enabled automatically and the bot generates short-lived SigV4 presigned tokens on each connection.

```bash
# Bot secret (IAM auth)
aws secretsmanager create-secret \
  --name deploy-bot/bot-secrets \
  --secret-string '{
    "slack_bot_token":              "xoxb-...",
    "github_token":                 "github_pat_...",
    "redis_addr":                   "deploy-bot.xxxxxx.ng.0001.use1.cache.amazonaws.com:6379",
    "redis_iam_auth":               true,
    "redis_user_id":                "deploy-bot-iam",
    "redis_replication_group_id":   "deploy-bot"
  }'

# Receiver secret (IAM auth)
aws secretsmanager create-secret \
  --name deploy-bot/receiver-secrets \
  --secret-string '{
    "slack_bot_token":              "xoxb-...",
    "slack_app_token":              "xapp-...",
    "github_token":                 "github_pat_...",
    "github_scanner_token":         "github_pat_...",
    "redis_addr":                   "deploy-bot.xxxxxx.ng.0001.use1.cache.amazonaws.com:6379",
    "redis_iam_auth":               true,
    "redis_user_id":                "deploy-bot-iam",
    "redis_replication_group_id":   "deploy-bot"
  }'
```

The `redis_user_id` must match the ElastiCache user ID configured with IAM authentication type. The `redis_replication_group_id` is the ElastiCache replication group ID (not the endpoint address) — it is used as the Host in the SigV4 presigned request. The IAM role or user running the bot/receiver must have `elasticache:Connect` permission on both the replication group and user ARNs (the Terraform module handles this when `elasticache_replication_group_arn` and `elasticache_user_arn` are set).

See [`terraform/examples/elasticache/`](https://github.com/ezubriski/deploy-bot/tree/main/terraform/examples/elasticache) for a reference ElastiCache setup with IAM auth, encryption, and automatic failover.

## Config file (`config.json`)

### GitHub

| Field | Default | Description |
|---|---|---|
| `github.org` | | GitHub organisation |
| `github.repo` | | GitOps repository name |
| `github.deployer_team` | | GitHub team slug -- members can request deploys |
| `github.approver_team` | | GitHub team slug -- members can approve/reject |
| `github.users` | `{}` | Optional map of Slack user ID to GitHub login for users with private GitHub emails (e.g. `{"U12345": "ghlogin"}`) |
| `github.rate_limit_max_retries` | `3` | Max retries on GitHub secondary rate limit |
| `github.rate_limit_retry_wait` | `"2m"` | Max wait between rate-limit retries |

### Slack

| Field | Default | Description |
|---|---|---|
| `slack.deploy_channel` | | Channel where deployment notifications are posted |
| `slack.allowed_channels` | `[]` (all) | Channel IDs where `/deploy` is accepted. Omit to allow all. Use IDs (`C01234567`), not names |
| `slack.buffer_size` | `500` | Events buffered in memory when Redis is unavailable. Buffered events are retried with backoff; Slack retries in parallel since they are never ACKed from the buffer |
| `slack.thread_threshold` | `4` | Controls when the bot threads deploy notifications under a parent message per environment. `0` or omitted: default threshold of 4 (thread when 4+ deploys are pending in the same environment). `-1`: never thread. `1`: always thread |
| `slack.rate_limit_max_retries` | `3` | Max retries on Slack 429 responses |
| `slack.rate_limit_retry_wait` | `"30s"` | Max wait between Slack rate-limit retries |

### Deployment

| Field | Default | Description |
|---|---|---|
| `deployment.stale_duration` | `"2h"` | How long a pending deploy waits before the sweeper expires it |
| `deployment.merge_method` | `"squash"` | `squash`, `merge`, or `rebase` |
| `deployment.lock_ttl` | `"5m"` | Per-app deploy lock duration |
| `deployment.label` | `"deploy-bot"` | GitHub label applied to every deploy PR. Used to rediscover open PRs after a Redis flush |
| `deployment.reconcile_interval` | disabled | If set (e.g. `"1h"`), periodically reconcile open labeled PRs against Redis state. Startup reconciliation always runs regardless |
| `deployment.allow_prod_auto_deploy` | `false` | If false, `auto_deploy: true` is ignored for apps whose environment is `prod` or `production` |

### AWS

| Field | Default | Description |
|---|---|---|
| `aws.ecr_role_arn` | | Optional role to assume for ECR reads. Omit to use pod identity directly |
| `aws.ecr_region` | | Region of app ECR repositories |
| `aws.audit_role_arn` | | Optional role to assume for S3 audit writes. Omit to use pod identity directly |
| `aws.audit_region` | | Region of the audit S3 bucket |
| `aws.audit_bucket` | | S3 bucket for audit logs. If empty, audit events are written to the application log via zap instead of S3 |

### ECR Events (ECR push-triggered deploys)

Disabled by default. When `sqs_queue_url` is set, the receiver polls an SQS queue for ECR push events from EventBridge and enqueues matching deploys.

| Field | Default | Description |
|---|---|---|
| `ecr_events.sqs_queue_url` | `""` (disabled) | SQS queue URL to poll for ECR push events |
| `ecr_events.poll_interval` | `"30s"` | How often to long-poll the SQS queue |

See [ecr-push-triggered-deploys.md](ecr-push-triggered-deploys.md) for the full design.

### Repo Discovery (repo-sourced app configuration)

Disabled by default. When enabled, the receiver scans GitHub repos for `.deploy-bot.json` files and merges discovered apps into the config. Operator-managed entries always take precedence.

| Field | Default | Description |
|---|---|---|
| `repo_discovery.enabled` | `false` | Enable repo-sourced app discovery |
| `repo_discovery.poll_interval` | `"5m"` | How often to scan repos |
| `repo_discovery.config_file` | `".deploy-bot.json"` | File to look for in each repo. Configurable because the bot may be installed under a different name |
| `repo_discovery.repo_prefix` | `""` (all) | Only scan repos whose name starts with this prefix |
| `repo_discovery.discovered_path` | `"/etc/deploy-bot/discovered.json"` | Where the bot reads merged discovered apps |
| `repo_discovery.configmap_name` | `"deploy-bot-discovered"` | ConfigMap to write discovered apps to |
| `repo_discovery.configmap_namespace` | inferred from pod | Namespace of the ConfigMap |
| `repo_discovery.rate_limit_floor` | `500` | Pause scanning when GitHub rate limit remaining drops below this |
| `repo_discovery.warn_channel` | deploy channel | Slack channel for conflict warnings |

See [repo-sourced-app-discovery.md](repo-sourced-app-discovery.md) for the full design.

### Apps

Each entry in the `apps[]` array defines one deployable application. App names include the environment (e.g. `myapp-dev`, `myapp-prod`) and must be unique.

| Field | Default | Description |
|---|---|---|
| `app` | | App name (including environment, e.g. `myapp-prod`). Must be unique across all entries |
| `environment` | | **Required.** Environment name (e.g. `dev`, `prod`). Included in lock keys, branch names, PR titles, and all Slack messages |
| `kustomize_path` | | Path to the kustomization.yaml in the gitops repo |
| `ecr_repo` | | Full ECR repository URI (e.g. `123456789.dkr.ecr.us-east-1.amazonaws.com/myapp`) |
| `tag_pattern` | | Regex pattern for valid tags. Tags not matching are rejected by the modal and filtered by the ECR poller |
| `auto_deploy` | `false` | When true, ECR push events for this app deploy automatically without human approval. Subject to `allow_prod_auto_deploy` |
| `auto_deploy_approver_group` | | Slack ID to notify for ECR-triggered deploys. Channel ID (`C...`) posts there directly; user group ID (`S...`) mentions the group in deploy channel |

Example:

```json
{
  "app": "myapp-prod",
  "environment": "prod",
  "kustomize_path": "apps/myapp/overlays/prod/kustomization.yaml",
  "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/myapp",
  "tag_pattern": "^v[0-9]+\\.[0-9]+\\.[0-9]+$",
  "auto_deploy": false
}
```

## Repo-sourced app config (`.deploy-bot.json`)

Repositories can declare their own app entries by placing a JSON file (default `.deploy-bot.json`) in the repo root on the default branch. The `apiVersion` field is required.

**Recommended (v2)** -- with `enforce_repo_naming` enabled, app name and kustomize path are derived from the repo name. Teams only specify what's unique to their app:

```json
{
  "apiVersion": "deploy-bot/v2",
  "apps": [
    {
      "environment": "dev",
      "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/myapp",
      "auto_deploy": true
    },
    {
      "environment": "prod",
      "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/myapp"
    }
  ]
}
```

**Flexible (v1)** -- all fields specified explicitly. Use without `enforce_repo_naming`, or for repos listed in `exempt_repos`:

```json
{
  "apiVersion": "deploy-bot/v1",
  "apps": [
    {
      "app": "myapp-dev",
      "environment": "dev",
      "kustomize_path": "apps/myapp/overlays/dev/kustomization.yaml",
      "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/myapp",
      "tag_pattern": "^v\\d+\\.\\d+\\.\\d+$",
      "auto_deploy": true
    },
    {
      "app": "myapp-prod",
      "environment": "prod",
      "kustomize_path": "apps/myapp/overlays/prod/kustomization.yaml",
      "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/myapp",
      "tag_pattern": "^v\\d+\\.\\d+\\.\\d+$",
      "auto_deploy": false
    }
  ]
}
```

See [repo-sourced-app-discovery.md](repo-sourced-app-discovery.md) for the full design including API version differences, naming conventions, and conflict handling.

The file name is configurable via `repo_discovery.config_file` to support installations where the bot has a different name.

Operator-managed apps always take precedence. If an `(app, environment)` pair exists in both operator config and a repo, the repo entry is discarded and a conflict warning is posted to Slack. Use `/deploy conflicts` or `@bot conflicts` to see current conflicts.

## Full example

```json
{
  "github": {
    "org": "myorg",
    "repo": "gitops-repo",
    "deployer_team": "deployers",
    "approver_team": "senior-engineers"
  },
  "slack": {
    "deploy_channel": "C01234567"
  },
  "deployment": {
    "stale_duration": "2h",
    "merge_method": "squash",
    "lock_ttl": "5m",
    "allow_prod_auto_deploy": false
  },
  "aws": {
    "ecr_region": "us-east-1",
    "audit_bucket": "my-audit-logs",
    "audit_region": "us-east-1"
  },
  "ecr_events": {
    "sqs_queue_url": "https://sqs.us-east-1.amazonaws.com/123456789012/deploy-bot-ecr-events",
    "poll_interval": "30s"
  },
  "repo_discovery": {
    "enabled": true,
    "poll_interval": "5m",
    "config_file": ".deploy-bot.json",
    "repo_prefix": ""
  },
  "apps": [
    {
      "app": "myapp-prod",
      "environment": "prod",
      "kustomize_path": "apps/myapp/overlays/prod/kustomization.yaml",
      "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/myapp",
      "tag_pattern": "^v[0-9]+\\.[0-9]+\\.[0-9]+$"
    }
  ]
}
```
