# Configuration Reference

The bot uses two configuration sources:

- **`config.json`** — mounted as a ConfigMap, hot-reloaded on file change (30s poll) or SIGHUP
- **`discovered.json`** — written by the repo scanner (optional), merged at load time

## Secrets (AWS Secrets Manager)

Create a secret at the path set in `AWS_SECRET_NAME` (default `deploy-bot/secrets`):

```bash
aws secretsmanager create-secret \
  --name deploy-bot/secrets \
  --secret-string '{
    "slack_bot_token": "xoxb-...",
    "slack_app_token": "xapp-...",
    "github_token":    "github_pat_...",
    "redis_addr":      "deploy-bot.xxxxxx.ng.0001.use1.cache.amazonaws.com:6379",
    "redis_token":     "your-elasticache-auth-token"
  }'
```

| Field | Required | Where to find it |
|---|---|---|
| `slack_bot_token` | Yes | Slack App > OAuth & Permissions > Bot User OAuth Token (`xoxb-`) |
| `slack_app_token` | Yes | Slack App > Basic Information > App-Level Tokens (`xapp-`) -- needs `connections:write` scope |
| `github_token` | Yes | GitHub > Settings > Developer settings > Fine-grained tokens -- scope to the gitops repo with Contents/Pull requests (read/write) and the org with Members (read) |
| `redis_addr` | Yes | ElastiCache > Cluster > Primary endpoint (include port, typically `6379`) |
| `redis_token` | No | ElastiCache > Cluster > Auth token -- only set if in-transit encryption with token auth is enabled |

To rotate a value without touching the others:

```bash
aws secretsmanager get-secret-value --secret-id deploy-bot/secrets \
  --query SecretString --output text | \
  jq '.github_token = "github_pat_newtoken"' | \
  xargs -0 aws secretsmanager put-secret-value \
    --secret-id deploy-bot/secrets --secret-string
```

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

Repositories can declare their own app entries by placing a JSON file (default `.deploy-bot.json`) in the repo root on the default branch:

```json
{
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
