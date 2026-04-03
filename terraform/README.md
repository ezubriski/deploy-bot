# deploy-bot Terraform module

Creates the AWS resources needed to run deploy-bot:

- **Two IAM roles** with least-privilege separation: bot and receiver (IRSA trust policy optional)
- **Two managed IAM policies** (always created) that can be attached to roles or IAM users
- **SQS queue + EventBridge rule** (optional) for ECR push-triggered deploys

## Files

| File | Contents |
|---|---|
| `main.tf` | Provider config, locals, SQS queue, EventBridge rule/target, queue policy |
| `bot_role.tf` | Bot IAM role (conditional on IRSA), assume-role policy, managed policy (SecretsManager + ECR + S3 audit), policy attachment |
| `receiver_role.tf` | Receiver IAM role (conditional on IRSA), assume-role policy, managed policy (SecretsManager + SQS), policy attachment |
| `variables.tf` | All input variables |
| `outputs.tf` | Role ARNs, policy ARNs, SQS URL/ARN |

## Design

**Least-privilege split.** The bot and receiver run as separate pods with separate service accounts. The bot role grants SecretsManager read, ECR read (all repositories), and optionally S3 PutObject for audit logs. The receiver role grants SecretsManager read and, when ECR events are enabled, SQS consume permissions. This means a compromise of the receiver pod cannot read ECR or write audit logs, and vice versa.

**IRSA trust is optional.** IAM roles are always created. When `eks_oidc_provider_arn` and `eks_oidc_provider_url` are provided, the roles get an IRSA trust policy allowing the corresponding Kubernetes ServiceAccount to assume them. When omitted, the roles are created with an empty trust policy — configure trust separately to match your environment.

**Separate secrets.** Each component reads from its own Secrets Manager secret. The bot policy grants access only to `bot_secrets_manager_secret_name`; the receiver policy only to `receiver_secrets_manager_secret_name`. This prevents credential cross-access between components.

**Managed policies, not inline.** Policies are created as `aws_iam_policy` resources and attached via `aws_iam_role_policy_attachment`. This makes them visible in the IAM console policy list and reusable across principals.

**ECR events are optional.** Setting `ecr_events_enabled = true` creates the SQS queue, EventBridge rule, and adds SQS permissions to the receiver policy. When disabled (the default), none of these resources exist and the receiver policy only covers SecretsManager.

**S3 audit is optional.** Setting `audit_bucket` to a non-empty value adds an S3 PutObject statement to the bot policy scoped to that bucket. When empty (the default), the statement is omitted.

**Tags on everything.** `var.tags` is applied to all created resources.

## Variables

| Variable | Type | Default | Description |
|---|---|---|---|
| `name` | `string` | `"deploy-bot"` | Name prefix for all resources |
| `region` | `string` | (required) | AWS region |
| `account_id` | `string` | (required) | AWS account ID |
| `eks_oidc_provider_arn` | `string` | `""` | EKS OIDC provider ARN for IRSA (empty = no IRSA trust policy) |
| `eks_oidc_provider_url` | `string` | `""` | EKS OIDC provider URL (empty = no IRSA trust policy) |
| `namespace` | `string` | `"deploy-bot"` | Kubernetes namespace for IRSA trust policy |
| `bot_service_account_name` | `string` | `"deploy-bot-worker"` | Bot ServiceAccount name for IRSA trust policy |
| `receiver_service_account_name` | `string` | `"deploy-bot-receiver"` | Receiver ServiceAccount name for IRSA trust policy |
| `bot_secrets_manager_secret_name` | `string` | `"deploy-bot/bot-secrets"` | Secrets Manager secret for the bot (worker) component |
| `receiver_secrets_manager_secret_name` | `string` | `"deploy-bot/receiver-secrets"` | Secrets Manager secret for the receiver component |
| `audit_bucket` | `string` | `""` | S3 bucket for audit logs (empty = disable S3 statement in bot policy) |
| `ecr_events_enabled` | `bool` | `false` | Create SQS queue and EventBridge rule for ECR push events |
| `ecr_events_visibility_timeout` | `number` | `300` | SQS visibility timeout in seconds |
| `permissions_boundary` | `string` | `""` | IAM permissions boundary ARN to apply to created roles |
| `tags` | `map(string)` | `{}` | Tags applied to all resources |

## Outputs

| Output | Description |
|---|---|
| `bot_role_arn` | Bot IAM role ARN |
| `receiver_role_arn` | Receiver IAM role ARN |
| `bot_policy_arn` | Bot managed policy ARN (always created) |
| `receiver_policy_arn` | Receiver managed policy ARN (always created) |
| `sqs_queue_url` | SQS queue URL (empty string if ECR events disabled) |
| `sqs_queue_arn` | SQS queue ARN (empty string if ECR events disabled) |

## Usage

### With IRSA (EKS)

Both roles get an IRSA trust policy bound to their Kubernetes service accounts.

```hcl
module "deploy_bot" {
  source = "github.com/ezubriski/deploy-bot//terraform"

  region     = "us-east-1"
  account_id = "123456789012"

  eks_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
  eks_oidc_provider_url = "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"

  bot_secrets_manager_secret_name      = "deploy-bot/bot-secrets"
  receiver_secrets_manager_secret_name = "deploy-bot/receiver-secrets"
  audit_bucket                         = "my-audit-logs"

  tags = {
    Team = "platform"
  }
}
```

Annotate the Kubernetes service accounts with the role ARNs:

```yaml
# Bot ServiceAccount
metadata:
  annotations:
    eks.amazonaws.com/role-arn: <module.deploy_bot.bot_role_arn>

# Receiver ServiceAccount
metadata:
  annotations:
    eks.amazonaws.com/role-arn: <module.deploy_bot.receiver_role_arn>
```

### Without IRSA

Omit the OIDC variables. Roles are still created (with an empty trust policy) so you can configure trust separately to match your environment.

```hcl
module "deploy_bot" {
  source = "github.com/ezubriski/deploy-bot//terraform"

  region     = "us-east-1"
  account_id = "123456789012"

  bot_secrets_manager_secret_name      = "deploy-bot/bot-secrets"
  receiver_secrets_manager_secret_name = "deploy-bot/receiver-secrets"

  tags = {
    Team = "platform"
  }
}
```

### With ECR events enabled

Enable ECR push-triggered deploys by setting `ecr_events_enabled = true`. This creates the SQS queue and EventBridge rule.

```hcl
module "deploy_bot" {
  source = "github.com/ezubriski/deploy-bot//terraform"

  region     = "us-east-1"
  account_id = "123456789012"

  eks_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
  eks_oidc_provider_url = "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"

  bot_secrets_manager_secret_name      = "deploy-bot/bot-secrets"
  receiver_secrets_manager_secret_name = "deploy-bot/receiver-secrets"
  ecr_events_enabled                   = true

  tags = {
    Team = "platform"
  }
}
```

The EventBridge rule captures pushes across **all repositories** in the account. The bot filters events at runtime by matching the repository name against configured apps and validating tags against each app's `tag_pattern`. No Terraform changes are needed when adding new apps.

Set the `sqs_queue_url` output in your `config.json`:

```json
{
  "ecr_events": {
    "sqs_queue_url": "<module.deploy_bot.sqs_queue_url>"
  }
}
```

The SQS visibility timeout defaults to 300 seconds (5 minutes) to allow the buffer retry window to complete before SQS redelivers. Adjust with `ecr_events_visibility_timeout` if needed.
