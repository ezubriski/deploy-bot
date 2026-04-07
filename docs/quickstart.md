# Quickstart

Get deploy-bot running in under 30 minutes. This guide uses the minimum viable setup: IRSA roles, in-cluster Redis, no ECR events, no audit bucket. Good for evaluating the bot before committing to a full production setup.

For a hardened deployment with ElastiCache, WORM audit logging, encryption, and ECR push-triggered deploys, see [production-setup.md](production-setup.md).

## Prerequisites

- An EKS cluster with an OIDC provider configured
- An AWS account with `secretsmanager:CreateSecret` and `iam:*` permissions
- A GitHub organization with a gitops repo (kustomize-based)
- Admin access to a Slack workspace
- Terraform installed

## 1. Create the Slack app

1. Go to [api.slack.com/apps](https://api.slack.com/apps) → **Create New App → From a manifest**. Paste the contents of `slack-manifest.json` from this repo.
2. **Socket Mode** → Generate Token → name it `socket`, add `connections:write` scope. Copy the `xapp-` token.
3. **OAuth & Permissions** → Install to Workspace. Copy the `xoxb-` token.

## 2. Create a GitHub PAT

[Create a fine-grained PAT](https://github.com/settings/personal-access-tokens/new) scoped to your gitops repo:

| Permission | Level |
|---|---|
| Contents | Read & write |
| Pull requests | Read & write |
| Issues | Read & write |
| Commit statuses | Read & write |
| Members (org) | Read |

Validate it:

```bash
export GITHUB_ORG=your-org
export GITHUB_REPO=your-gitops-repo
export DEPLOY_BOT_TOKEN=github_pat_...
./scripts/validate-token.sh
```

## 3. Terraform

Grab your cluster's OIDC provider details:

```bash
# Get the OIDC issuer URL for your cluster
OIDC_URL=$(aws eks describe-cluster --name your-cluster --query "cluster.identity.oidc.issuer" --output text)

# Derive the ARN and URL (without https://) from the issuer
OIDC_ID=$(echo "$OIDC_URL" | sed 's|https://||')
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
OIDC_ARN="arn:aws:iam::${ACCOUNT_ID}:oidc-provider/${OIDC_ID}"

echo "eks_oidc_provider_arn = \"${OIDC_ARN}\""
echo "eks_oidc_provider_url = \"${OIDC_ID}\""
```

Create a `main.tf` using the values above:

```hcl
module "deploy_bot" {
  source = "github.com/ezubriski/deploy-bot//terraform"

  region     = "us-east-1"
  account_id = "123456789012"

  eks_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
  eks_oidc_provider_url = "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"

  tags = { Project = "deploy-bot" }
}

output "bot_role_arn"        { value = module.deploy_bot.bot_role_arn }
output "receiver_role_arn"   { value = module.deploy_bot.receiver_role_arn }
output "bot_secret_arn"      { value = module.deploy_bot.bot_secret_arn }
output "receiver_secret_arn" { value = module.deploy_bot.receiver_secret_arn }
```

Apply:

```bash
terraform init && terraform apply
```

This creates:
- Two IAM roles with IRSA trust policies (bot, receiver) with least-privilege
- Two Secrets Manager secrets (empty — you'll populate them next)

## 4. Populate secrets

```bash
# Bot secret
aws secretsmanager put-secret-value \
  --secret-id deploy-bot/bot-secrets \
  --secret-string '{
    "slack_bot_token": "xoxb-...",
    "github_token":    "github_pat_...",
    "redis_addr":      "redis.deploy-bot.svc.cluster.local:6379"
  }'

# Receiver secret
aws secretsmanager put-secret-value \
  --secret-id deploy-bot/receiver-secrets \
  --secret-string '{
    "slack_bot_token": "xoxb-...",
    "slack_app_token": "xapp-...",
    "github_token":    "github_pat_...",
    "redis_addr":      "redis.deploy-bot.svc.cluster.local:6379"
  }'
```

## 5. Write your config

Create `config.json`:

```json
{
  "github": {
    "org": "your-org",
    "repo": "your-gitops-repo"
  },
  "authorization": [
    {"type": "github_teams", "value": ["developers"]}
  ],
  "slack": {
    "deploy_channel": "C0123456789"
  },
  "aws": {
    "ecr_region": "us-east-1"
  },
  "apps": [
    {
      "app": "myapp",
      "environment": "dev",
      "kustomize_path": "apps/myapp/overlays/dev/kustomization.yaml",
      "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/myapp"
    }
  ]
}
```

## 6. Deploy

The `deploy/` directory is a Kustomize base that includes in-cluster Redis. Create an overlay that wires up IRSA:

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

patches:
  # Add IRSA annotation to worker ServiceAccount
  - target:
      kind: ServiceAccount
      name: deploy-bot-worker
    patch: |
      - op: add
        path: /metadata/annotations
        value:
          eks.amazonaws.com/role-arn: "<bot_role_arn from terraform>"
  # Add IRSA annotation to receiver ServiceAccount
  - target:
      kind: ServiceAccount
      name: deploy-bot-receiver
    patch: |
      - op: add
        path: /metadata/annotations
        value:
          eks.amazonaws.com/role-arn: "<receiver_role_arn from terraform>"
```

Apply:

```bash
kustomize build overlay/ | kubectl apply -f -
```

## 7. Verify

```
/deploy help     — should show command help
/deploy apps     — should list your app
```

Try a deploy: `/deploy myapp-dev` → pick a tag → approve it → watch the PR merge.

## Next steps

- Add more apps to `config.json` (hot-reloaded, no restart needed)
- Enable ECR push-triggered deploys and repo-sourced discovery
- Move to a [production setup](production-setup.md) with ElastiCache, audit logging, and encryption
