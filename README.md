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
- ElastiCache for Redis — Multi-AZ with automatic failover and AOF persistence enabled
- GitHub App or PAT with repo and team read permissions
- Slack App in Socket Mode with the following bot scopes:
  `commands`, `chat:write`, `users:read`, `users:read.email`, `im:write`

## Configuration

### Secrets (AWS Secrets Manager)

Create a secret (JSON) at the path set in `AWS_SECRET_NAME`:

```json
{
  "slack_bot_token":  "xoxb-...",
  "slack_app_token":  "xapp-...",
  "github_token":     "ghp_...",
  "redis_addr":       "your-cluster.cache.amazonaws.com:6379"
}
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
| `deployment.stale_duration` | How long a pending deploy waits before expiring (default `2h`) |
| `deployment.merge_method` | `squash`, `merge`, or `rebase` (default `squash`) |
| `deployment.lock_ttl` | Per-app lock duration (default `5m`) |
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

The ServiceAccount needs an IAM role (set via the IRSA annotation in `deploy/rbac.yaml`) with:

- `secretsmanager:GetSecretValue` on the bot's secret
- `ecr:GetAuthorizationToken`, `ecr:DescribeImages`, `ecr:BatchGetImage` (via `ecr_role_arn` assume)
- `s3:PutObject` on the audit bucket (via `audit_role_arn` assume)
- `sts:AssumeRole` for the above two roles

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
