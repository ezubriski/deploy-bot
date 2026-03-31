# deploy-bot

A Slack bot that gates Kubernetes deployments behind an approval workflow. Developers request deployments via `/deploy`, approvers approve or reject via Slack buttons, and the bot creates and merges GitHub PRs that update kustomize image tags in a GitOps repo. Argo CD picks up merged PRs and deploys.

## How it works

```
Developer          Receiver          Redis Stream       Worker            GitHub / Argo CD
    |                  |                   |               |                     |
    |-- /deploy ------>|                   |               |                     |
    |                  |-- enqueue ------->|               |                     |
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
```

Two processes share a single container image:

- **receiver** — connects to Slack via Socket Mode, validates incoming events, and enqueues them to a Redis Stream. Stateless; run 2+ replicas.
- **worker** — consumes events from the stream, runs all business logic (GitHub API, ECR, audit logging). Run 2+ replicas; Redis Streams consumer groups ensure each event is processed once.

## Prerequisites

- Kubernetes cluster (EKS recommended)
- AWS — Secrets Manager, ECR (for app images), S3 (audit log), STS (cross-account roles)
- ElastiCache for Redis — Multi-AZ with automatic failover and AOF persistence enabled. **Redis is required; the bot will not start without it.** See [docs/redis-resilience.md](docs/redis-resilience.md) for behaviour during outages and after a flush.
- GitHub fine-grained PAT with repository (contents, pull requests) and organisation (members read) permissions
- Slack App in Socket Mode with the following bot scopes:
  `commands`, `chat:write`, `users:read`, `users:read.email`, `im:write`

## Configuration

### Secrets (AWS Secrets Manager)

Create a secret at the path set in `AWS_SECRET_NAME` (default `deploy-bot/secrets`):

```bash
aws secretsmanager create-secret \
  --name deploy-bot/secrets \
  --secret-string '{
    "slack_bot_token": "xoxb-111111111111-2222222222222-xxxxxxxxxxxxxxxxxxxxxxxx",
    "slack_app_token": "xapp-1-Axxxxxxxxxx-2222222222222-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
    "github_token":    "github_pat_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
    "redis_addr":      "deploy-bot.xxxxxx.ng.0001.use1.cache.amazonaws.com:6379",
    "redis_token":     "your-elasticache-auth-token"
  }'
```

`redis_token` is optional. Omit the field entirely if your ElastiCache cluster does not require authentication.

| Field | Required | Where to find it |
|---|---|---|
| `slack_bot_token` | Yes | Slack App → OAuth & Permissions → Bot User OAuth Token (`xoxb-`) |
| `slack_app_token` | Yes | Slack App → Basic Information → App-Level Tokens (`xapp-`) — needs `connections:write` scope |
| `github_token` | Yes | GitHub → Settings → Developer settings → Fine-grained tokens — scope to the gitops repo with Contents/Pull requests (read/write) and the org with Members (read) |
| `redis_addr` | Yes | ElastiCache → Cluster → Primary endpoint (include port, typically `6379`) |
| `redis_token` | No | ElastiCache → Cluster → Auth token — only set if in-transit encryption with token auth is enabled |

To rotate a value without touching the others:

```bash
aws secretsmanager get-secret-value --secret-id deploy-bot/secrets \
  --query SecretString --output text | \
  jq '.github_token = "github_pat_newtoken"' | \
  xargs -0 aws secretsmanager put-secret-value \
    --secret-id deploy-bot/secrets --secret-string
```

### Config file (`config.json`)

Mounted as a ConfigMap. Hot-reloaded on SIGHUP or file change (30s poll). See `deploy/configmap.yaml` for a full example.

| Field | Description |
|---|---|
| `github.org` | GitHub organisation |
| `github.repo` | GitOps repository name |
| `github.deployer_team` | GitHub team slug — members can request deploys |
| `github.approver_team` | GitHub team slug — members can approve/reject |
| `slack.deploy_channel` | Channel where deployment notifications are posted |
| `slack.allowed_channels` | Optional list of channel IDs where `/deploy` commands are accepted. Omit or leave empty to allow all channels. Use channel IDs (e.g. `C01234567`), not names |
| `slack.buffer_size` | Number of events the receiver buffers in memory when Redis is unavailable (default `500`). Buffered events are retried with exponential backoff until Redis recovers. Events are never ACKed to Slack from the buffer — Slack retries in parallel |
| `deployment.stale_duration` | How long a pending deploy waits before expiring (default `2h`) |
| `deployment.merge_method` | `squash`, `merge`, or `rebase` (default `squash`) |
| `deployment.lock_ttl` | Per-app lock duration (default `5m`) |
| `deployment.label` | GitHub label applied to every deploy PR (default `deploy-bot`). Used to rediscover open PRs after a Redis flush |
| `deployment.reconcile_interval` | If set (e.g. `1h`), periodically reconcile open labeled PRs against Redis state. Disabled by default; startup reconciliation always runs |
| `aws.ecr_role_arn` | Role to assume for reading app ECR repositories |
| `aws.ecr_region` | Region of app ECR repositories |
| `aws.audit_role_arn` | Role to assume for writing audit logs to S3 |
| `aws.audit_bucket` | S3 bucket for audit logs |
| `apps[]` | One entry per deployable application (see below) |

Each app entry:

```json
{
  "app": "myapp",
  "kustomize_path": "apps/myapp/kustomization.yaml",
  "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/myapp",
  "tag_pattern": "^v[0-9]+\\.[0-9]+\\.[0-9]+$"
}
```

## Deployment

### IAM

The bot uses three IAM roles:

**Bot role** — attached to the `deploy-bot` Kubernetes ServiceAccount via IRSA (`deploy/rbac.yaml`). Needs permission to read its own secret and to assume the two cross-account roles:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ReadBotSecret",
      "Effect": "Allow",
      "Action": "secretsmanager:GetSecretValue",
      "Resource": "arn:aws:secretsmanager:<region>:<account>:secret:deploy-bot/secrets-*"
    },
    {
      "Sid": "AssumeServiceRoles",
      "Effect": "Allow",
      "Action": "sts:AssumeRole",
      "Resource": [
        "<ecr_role_arn>",
        "<audit_role_arn>"
      ]
    }
  ]
}
```

**ECR role** (`ecr_role_arn`) — assumed by the worker to read app image repositories. Attach this trust policy to the role so the bot role can assume it:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "AWS": "<bot-role-arn>" },
    "Action": "sts:AssumeRole"
  }]
}
```

And grant these permissions:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Sid": "ReadECR",
    "Effect": "Allow",
    "Action": [
      "ecr:GetAuthorizationToken",
      "ecr:DescribeImages",
      "ecr:BatchGetImage",
      "ecr:ListImages"
    ],
    "Resource": "*"
  }]
}
```

> `ecr:GetAuthorizationToken` requires `Resource: "*"` — it cannot be scoped to a repository ARN.

**Audit role** (`audit_role_arn`) — assumed by the worker to write audit log entries. Same trust policy pattern as above, with:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Sid": "WriteAuditLog",
    "Effect": "Allow",
    "Action": "s3:PutObject",
    "Resource": "arn:aws:s3:::<audit_bucket>/deploy-bot/*"
  }]
}
```

### Slack app setup

Use the `slack-manifest.json` file at the root of this repository to create
the app in one step:

1. Go to [api.slack.com/apps](https://api.slack.com/apps) and click
   **Create New App → From a manifest**. Select your workspace, paste the
   contents of `slack-manifest.json`, and click through to create the app.

2. Go to **Socket Mode** in the sidebar. You will see Socket Mode is already
   enabled. Click **Generate Token**, name it (e.g. `socket`), add the
   `connections:write` scope, and click **Generate**. Copy the token (starts
   with `xapp-`) — this is your `slack_app_token`.

3. Go to **OAuth & Permissions** and click **Install to Workspace**. Approve
   the permissions. Copy the **Bot User OAuth Token** (starts with `xoxb-`) —
   this is your `slack_bot_token`.

### GitHub permissions

A **fine-grained Personal Access Token** is required. Store it in the `github_token` secret field.

Create it at GitHub → Settings → Developer settings → Personal access tokens → Fine-grained tokens. Set the resource owner to your organisation (not your personal account) so that organisation permissions can be granted.

**Repository permissions** — scope to the gitops repo only:

| Permission | Level | Why |
|---|---|---|
| Contents | Read & write | Push kustomization branches |
| Pull requests | Read & write | Create, merge, close PRs and post comments |

**Organisation permissions:**

| Permission | Level | Why |
|---|---|---|
| Members | Read | Check deployer/approver team membership |

If your organisation requires administrator approval for fine-grained tokens with organisation permissions (`Settings → Personal access tokens → Require administrator approval`), the token will be pending until an org admin approves it.

### Apply manifests

```bash
kubectl create namespace deploy-bot
kubectl apply -f deploy/
```

Update the image tag in `deploy/deployment.yaml` to match the version you want to run. The ECR image is:

```
123456789012.dkr.ecr.us-west-2.amazonaws.com/deploy-bot:<tag>
```

### Endpoints

| Process | Port | Paths |
|---|---|---|
| worker | 9090 | `/healthz` (liveness), `/readyz` (readiness), `/metrics` (Prometheus) |
| receiver | 8080 | `/healthz` (liveness) |

The worker's `/readyz` returns 503 until the ECR cache has completed its first populate. The receiver's `/healthz` is always 200 if the process is running.

## Slack commands

| Command | Description |
|---|---|
| `/deploy` | Open the deployment request modal |
| `/deploy <app>` | Open the modal pre-selected to an app |
| `/deploy status` | List all pending deployments |
| `/deploy history [app]` | Show recent completed deployments |
| `/deploy tags <app>` | List the 20 most recent valid tags for an app |
| `/deploy tags <app> <tag>` | Verify a specific tag exists in ECR |
| `/deploy cancel <pr>` | Cancel your own pending deployment |
| `/deploy nudge <pr>` | Re-ping the approver |
| `/deploy rollback <app>` | Re-deploy the previous approved tag |

## Development

```bash
make build        # build both binaries to ./bin
make test         # run all tests
make lint         # run golangci-lint
make docker-build # build image (tagged with git short SHA)
make ecr-login    # authenticate Docker to ECR
make docker-push  # build and push to ECR
make clean        # remove ./bin
```

Override the image tag: `make docker-push TAG=v1.2.3`

## Monitoring

Worker pods expose Prometheus metrics on port `9090` at `/metrics` and are annotated for auto-discovery:

```yaml
prometheus.io/scrape: "true"
prometheus.io/port:   "9090"
prometheus.io/path:   "/metrics"
```

If your Prometheus is configured with Kubernetes pod auto-discovery, the following scrape job will pick up any pod across the cluster that carries these annotations — not just deploy-bot:

```yaml
scrape_configs:
  - job_name: kubernetes-annotated-pods
    kubernetes_sd_configs:
      - role: pod
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape]
        action: keep
        regex: "true"
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_path]
        action: replace
        target_label: __metrics_path__
        regex: (.+)
      - source_labels: [__address__, __meta_kubernetes_pod_annotation_prometheus_io_port]
        action: replace
        regex: ([^:]+)(?::\d+)?;(\d+)
        replacement: $1:$2
        target_label: __address__
      - source_labels: [__meta_kubernetes_pod_label_app]
        action: replace
        target_label: app
      - source_labels: [__meta_kubernetes_namespace]
        action: replace
        target_label: namespace
```

If you are running the Prometheus Operator, use a `ServiceMonitor` instead:

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

GitHub Actions builds and pushes to ECR on every push to `main` (tagged with the short SHA and `latest`) and on version tags (`v*`, tagged with the version). Requires `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` repository secrets scoped to the `deploy-bot` ECR repository.
