# Production Setup

A step-by-step guide for deploying deploy-bot with production-grade infrastructure: IRSA, ElastiCache with IAM auth, WORM-compliant audit logging, encrypted secrets, ECR push-triggered deploys, and repo-sourced app discovery.

If you're evaluating the bot, start with the [quickstart](quickstart.md) and come back here when you're ready to harden.

## Prerequisites

- An EKS cluster with an OIDC provider configured
- Terraform, `kubectl`, and the AWS CLI
- A GitHub organization with a gitops repo
- Admin access to a Slack workspace

## 1. Create the Slack app

1. Go to [api.slack.com/apps](https://api.slack.com/apps) → **Create New App → From a manifest**. Paste the contents of `slack-manifest.json` from this repo.
2. **Socket Mode** → Generate Token → name it `socket`, add `connections:write` scope. Copy the `xapp-` token.
3. **OAuth & Permissions** → Install to Workspace. Copy the `xoxb-` token.

## 2. Create GitHub PATs

You need two tokens: one for the gitops repo (read/write) and one for repo discovery (read-only, broader scope).

**Primary token** — [create here](https://github.com/settings/personal-access-tokens/new), scoped to the gitops repo:

| Permission | Level |
|---|---|
| Contents | Read & write |
| Pull requests | Read & write |
| Issues | Read & write |
| Commit statuses | Read & write |
| Members (org) | Read |

**Scanner token** (optional) — [create here](https://github.com/settings/personal-access-tokens/new), scoped to all repos (or repos with your discovery prefix):

| Permission | Level |
|---|---|
| Contents | Read |
| Commit statuses | Read & write |

Validate:

```bash
export GITHUB_ORG=your-org
export GITHUB_REPO=your-gitops-repo
export DEPLOY_BOT_TOKEN=github_pat_...
export DEPLOY_BOT_SCANNER_TOKEN=github_pat_...
./scripts/validate-token.sh
```

## 3. Terraform

This example sets up the full infrastructure: IRSA roles, Secrets Manager with a CMK, audit bucket with WORM compliance, SQS/EventBridge for ECR events, and ElastiCache IAM auth.

### 3a. ElastiCache (optional)

A reference module is provided at `terraform/examples/elasticache/`. Copy it into your infrastructure repo and adapt:

```hcl
module "redis" {
  source = "./modules/elasticache"  # your copy

  name               = "deploy-bot"
  subnet_ids         = ["subnet-abc", "subnet-def"]
  security_group_ids = ["sg-123"]

  default_user_password = random_password.redis_default.result
}

resource "random_password" "redis_default" {
  length  = 32
  special = true
}
```

This creates an ElastiCache replication group with:
- IAM authentication (no static passwords for the bot)
- Encryption in transit (TLS) and at rest (KMS)
- Multi-AZ automatic failover
- Automatic snapshots with 7-day retention

### 3b. deploy-bot module

Grab your cluster's OIDC provider details:

```bash
OIDC_URL=$(aws eks describe-cluster --name your-cluster --query "cluster.identity.oidc.issuer" --output text)
OIDC_ID=$(echo "$OIDC_URL" | sed 's|https://||')
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
OIDC_ARN="arn:aws:iam::${ACCOUNT_ID}:oidc-provider/${OIDC_ID}"

echo "eks_oidc_provider_arn = \"${OIDC_ARN}\""
echo "eks_oidc_provider_url = \"${OIDC_ID}\""
```

```hcl
module "deploy_bot" {
  source = "github.com/ezubriski/deploy-bot//terraform"

  region     = "us-east-1"
  account_id = "123456789012"

  # IRSA
  eks_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
  eks_oidc_provider_url = "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"

  # Secrets (CMK encryption)
  secrets_kms_key_arn = aws_kms_key.deploy_bot.arn

  # Audit bucket (WORM, 3-year retention, Glacier lifecycle)
  create_audit_bucket            = true
  audit_bucket_kms_key_arn       = aws_kms_key.deploy_bot.arn
  create_audit_access_log_bucket = true

  # ECR push-triggered deploys
  ecr_events_enabled = true
  sqs_kms_key_arn    = aws_kms_key.deploy_bot.arn

  # ElastiCache IAM auth (omit if using in-cluster Redis)
  elasticache_replication_group_arn = module.redis.replication_group_arn
  elasticache_user_arn             = module.redis.iam_user_arn

  tags = {
    Team        = "platform"
    Environment = "production"
  }
}

resource "aws_kms_key" "deploy_bot" {
  description         = "deploy-bot encryption key"
  enable_key_rotation = true
}
```

Apply:

```bash
terraform init && terraform apply
```

### What this creates

| Resource | Purpose |
|---|---|
| 2 IAM roles (IRSA) | Least-privilege bot and receiver identities |
| 2 managed IAM policies | SecretsManager, ECR, S3, SQS, ElastiCache, KMS |
| 2 Secrets Manager secrets | Bot and receiver credentials (CMK-encrypted) |
| S3 audit bucket | WORM (Object Lock, compliance mode, 3-year retention) |
| S3 access log bucket | Tracks access to the audit bucket |
| SQS queue | ECR push events from EventBridge (SSE-SQS or CMK) |
| EventBridge rule | Captures all ECR push events account-wide |

## 4. Populate secrets

### With ElastiCache IAM auth

```bash
# Bot secret
aws secretsmanager put-secret-value \
  --secret-id deploy-bot/bot-secrets \
  --secret-string '{
    "slack_bot_token":              "xoxb-...",
    "github_token":                 "github_pat_...",
    "redis_addr":                   "deploy-bot.xxxxxx.ng.0001.use1.cache.amazonaws.com:6379",
    "redis_iam_auth":               true,
    "redis_user_id":                "deploy-bot-iam",
    "redis_replication_group_id":   "deploy-bot"
  }'

# Receiver secret
aws secretsmanager put-secret-value \
  --secret-id deploy-bot/receiver-secrets \
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

The `redis_user_id` must match the ElastiCache user configured with IAM authentication. The `redis_replication_group_id` is the replication group ID (not the endpoint) — it's used to generate SigV4 presigned auth tokens. TLS is enabled automatically when `redis_iam_auth` is true.

### With password auth (non-ElastiCache Redis)

```bash
# Bot secret
aws secretsmanager put-secret-value \
  --secret-id deploy-bot/bot-secrets \
  --secret-string '{
    "slack_bot_token": "xoxb-...",
    "github_token":    "github_pat_...",
    "redis_addr":      "your-redis:6379",
    "redis_token":     "your-auth-token"
  }'

# Receiver secret
aws secretsmanager put-secret-value \
  --secret-id deploy-bot/receiver-secrets \
  --secret-string '{
    "slack_bot_token":      "xoxb-...",
    "slack_app_token":      "xapp-...",
    "github_token":         "github_pat_...",
    "github_scanner_token": "github_pat_...",
    "redis_addr":           "your-redis:6379",
    "redis_token":          "your-auth-token"
  }'
```

## 5. Write your config

```json
{
  "github": {
    "org": "your-org",
    "repo": "your-gitops-repo",
    "deployer_team": "developers",
    "approver_team": "senior-engineers"
  },
  "slack": {
    "deploy_channel": "C0123456789"
  },
  "deployment": {
    "stale_duration": "2h",
    "merge_method": "squash",
    "lock_ttl": "5m",
    "label": "deploy-bot",
    "allow_prod_auto_deploy": false
  },
  "aws": {
    "ecr_region": "us-east-1",
    "audit_bucket": "<terraform audit_bucket_name output>",
    "audit_region": "us-east-1"
  },
  "ecr_events": {
    "sqs_queue_url": "<terraform sqs_queue_url output>"
  },
  "repo_discovery": {
    "enabled": true,
    "poll_interval": "5m",
    "enforce_repo_naming": true,
    "kustomize_path_template": "{env}/{repo}/kustomization.yaml",
    "default_tag_pattern": "^v[0-9]+\\.[0-9]+\\.[0-9]+$"
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

See [configuration.md](configuration.md) for the full reference.

## 6. Deploy

Create a Kustomize overlay. For production, remove the in-cluster Redis (you're using ElastiCache) and add IRSA annotations:

```yaml
# overlay/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  - github.com/ezubriski/deploy-bot/deploy

images:
  - name: ghcr.io/ezubriski/deploy-bot
    newTag: v1.0.0  # pin to a release

configMapGenerator:
  - name: deploy-bot-config
    files:
      - config.json
    behavior: replace

generatorOptions:
  disableNameSuffixHash: true

patches:
  # Remove in-cluster Redis (using ElastiCache)
  - target:
      kind: Deployment
      name: redis
    patch: |
      $patch: delete
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: redis
  - target:
      kind: Service
      name: redis
    patch: |
      $patch: delete
      apiVersion: v1
      kind: Service
      metadata:
        name: redis

  # Add IRSA annotations
  - target:
      kind: Deployment
      name: deploy-bot-worker
    patch: |
      - op: add
        path: /spec/template/metadata/annotations/eks.amazonaws.com~1role-arn
        value: "<bot_role_arn from terraform>"
  - target:
      kind: Deployment
      name: deploy-bot-receiver
    patch: |
      - op: add
        path: /spec/template/metadata/annotations/eks.amazonaws.com~1role-arn
        value: "<receiver_role_arn from terraform>"
```

Apply:

```bash
kustomize build overlay/ | kubectl apply -f -
```

## 7. Verify

```
/deploy help     — command help
/deploy apps     — verify app config loaded
```

Deploy something: `/deploy myapp-prod` → pick a tag → approve → watch the PR merge and Argo CD sync.

## 8. Enable ECR push deploys

Already wired up from Terraform. To use it:

1. Set `auto_deploy: true` on apps that should deploy without approval.
2. Push an image to ECR matching the app's `ecr_repo` and `tag_pattern`.
3. The bot creates a PR and either auto-merges or requests approval.

See [ecr-push-triggered-deploys.md](ecr-push-triggered-deploys.md) for details.

## 9. Onboard app teams

With `repo_discovery` and `enforce_repo_naming` enabled, app teams add a two-line `.deploy-bot.json` to their repo:

```json
{
  "apiVersion": "deploy-bot/v2",
  "apps": [
    {"environment": "dev", "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-service"},
    {"environment": "prod", "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-service"}
  ]
}
```

App name and kustomize path are derived from the repo name. The bot discovers it on the next scan cycle. Add the [GitHub Action](../README.md#github-action) to their CI to validate configs in PRs.

See [naming-conventions.md](naming-conventions.md) for path templates, exemptions, and conflict resolution.

## Security checklist

| Item | Status |
|---|---|
| IRSA (no static AWS credentials on pods) | `eks_oidc_provider_arn` set |
| Separate IAM roles for bot and receiver | Default behavior |
| Secrets Manager (not env vars or configmaps) | `AWS_SECRET_NAME` set |
| Secrets encrypted with CMK | `secrets_kms_key_arn` set |
| ElastiCache IAM auth (no static Redis password) | `redis_iam_auth: true` |
| ElastiCache encryption in transit (TLS) | Automatic with IAM auth |
| ElastiCache encryption at rest (KMS) | Set in ElastiCache module |
| SQS encryption | `sqs_kms_key_arn` set |
| Audit bucket WORM (Object Lock, compliance mode) | `create_audit_bucket = true` |
| Audit bucket encryption | `audit_bucket_kms_key_arn` set |
| Audit bucket secure transport | Automatic (bucket policy) |
| Audit bucket access logging | `create_audit_access_log_bucket = true` |
| Audit retention (3 years, configurable) | `audit_bucket_retention_days = 1095` |
| No public network exposure | Socket Mode + SQS (no ingress) |
| Minimal container (FROM scratch) | Default Dockerfile |
| Non-root, read-only filesystem, caps dropped | Default manifests |
| GitHub PATs are fine-grained, least-privilege | Validated with `validate-token.sh` |
