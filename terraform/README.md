# deploy-bot Terraform module

Creates the AWS resources needed to run deploy-bot:

- **Two IAM roles** with least-privilege separation: bot and receiver (IRSA trust policy optional)
- **Two managed IAM policies** (always created) that can be attached to roles or IAM users
- **Secrets Manager secrets** (optional, default on) for bot and receiver credentials
- **S3 audit bucket** (optional) with WORM compliance, encryption, and secure transport
- **SQS queue + EventBridge rule** (optional) for ECR push-triggered deploys
- **ElastiCache IAM auth** permissions (optional) for passwordless Redis access

## Files

| File | Contents |
|---|---|
| `main.tf` | Provider config, locals, validation checks, SQS queue, EventBridge rule/target, queue policy |
| `bot_role.tf` | Bot IAM role (conditional on IRSA/EC2), assume-role policy, managed policy (SecretsManager + ECR + S3 audit + ElastiCache + KMS), policy attachment |
| `bot_user.tf` | Bot IAM user (conditional on identity_type), policy attachment |
| `receiver_role.tf` | Receiver IAM role (conditional on IRSA/EC2), assume-role policy, managed policy (SecretsManager + SQS + ElastiCache + KMS), policy attachment |
| `receiver_user.tf` | Receiver IAM user (conditional on identity_type), policy attachment |
| `secrets.tf` | Secrets Manager secrets for bot and receiver (optional) |
| `audit_bucket.tf` | S3 audit bucket with WORM, encryption, access logging, Glacier lifecycle (optional) |
| `variables.tf` | All input variables |
| `outputs.tf` | Role ARNs, policy ARNs, secret ARNs, bucket names, SQS URL/ARN |

## Design

**Least-privilege split.** The bot and receiver run as separate pods with separate service accounts. The bot role grants SecretsManager read, ECR read (all repositories), and optionally S3 PutObject for audit logs and ElastiCache Connect. The receiver role grants SecretsManager read and, when ECR events are enabled, SQS consume permissions plus ElastiCache Connect. A compromise of the receiver pod cannot read ECR or write audit logs, and vice versa.

**IRSA trust is optional.** When `eks_oidc_provider_arn` and `eks_oidc_provider_url` are provided, the roles get an IRSA trust policy. EC2 trust can be added with `enable_ec2_trust`. Alternatively, set `identity_type = "user"` to create IAM users instead of roles.

**Separate secrets.** Each component reads from its own Secrets Manager secret. The bot policy grants access only to `bot_secrets_manager_secret_name`; the receiver policy only to `receiver_secrets_manager_secret_name`. When `create_secrets = true` (the default), the module creates both secrets — populate them after apply. Optionally encrypt with a customer-managed KMS key via `secrets_kms_key_arn`.

**Managed policies, not inline.** Policies are created as `aws_iam_policy` resources and attached via `aws_iam_role_policy_attachment`. This makes them visible in the IAM console policy list and reusable across principals.

**Encryption everywhere.** SQS uses SSE-SQS by default (or a CMK via `sqs_kms_key_arn`). The audit bucket uses SSE-S3 by default (or a CMK via `audit_bucket_kms_key_arn`). Secrets Manager uses the AWS managed key by default (or a CMK via `secrets_kms_key_arn`). When CMKs are configured, the corresponding IAM policies automatically include the required KMS permissions.

**Audit bucket is optional.** Set `create_audit_bucket = true` to create a managed bucket with WORM compliance (Object Lock, compliance mode), 3-year default retention, Glacier lifecycle after 90 days, secure transport enforcement, and deletion resistance. Or set `audit_bucket` to reference an existing bucket. Access logging can target a created or existing bucket.

**ECR events are optional.** Setting `ecr_events_enabled = true` creates the SQS queue, EventBridge rule, and adds SQS permissions to the receiver policy. When disabled (the default), none of these resources exist.

**ElastiCache IAM auth is optional.** Setting `elasticache_replication_group_arn` and `elasticache_user_arn` adds `elasticache:Connect` to both bot and receiver policies. See [`examples/elasticache/`](examples/elasticache/) for a reference ElastiCache setup.

**Tags on everything.** `var.tags` is applied to all created resources.

## Variables

### Core

| Variable | Type | Default | Description |
|---|---|---|---|
| `name` | `string` | `"deploy-bot"` | Name prefix for all resources |
| `region` | `string` | (required) | AWS region |
| `account_id` | `string` | (required) | AWS account ID |
| `identity_type` | `string` | `"role"` | `"role"` or `"user"` — type of IAM identity to create |
| `permissions_boundary` | `string` | `""` | IAM permissions boundary ARN |
| `tags` | `map(string)` | `{}` | Tags applied to all resources |

### IAM trust (only when identity_type is "role")

| Variable | Type | Default | Description |
|---|---|---|---|
| `eks_oidc_provider_arn` | `string` | `""` | EKS OIDC provider ARN for IRSA |
| `eks_oidc_provider_url` | `string` | `""` | EKS OIDC provider URL |
| `namespace` | `string` | `"deploy-bot"` | Kubernetes namespace for IRSA trust policy |
| `bot_service_account_name` | `string` | `"deploy-bot-worker"` | Bot ServiceAccount name |
| `receiver_service_account_name` | `string` | `"deploy-bot-receiver"` | Receiver ServiceAccount name |
| `enable_ec2_trust` | `bool` | `false` | Add EC2 trust policy to roles |

### Secrets Manager

| Variable | Type | Default | Description |
|---|---|---|---|
| `create_secrets` | `bool` | `true` | Create Secrets Manager secrets (populate after apply) |
| `secrets_kms_key_arn` | `string` | `""` | CMK for secrets encryption (empty = AWS managed key) |
| `bot_secrets_manager_secret_name` | `string` | `"deploy-bot/bot-secrets"` | Bot secret name |
| `receiver_secrets_manager_secret_name` | `string` | `"deploy-bot/receiver-secrets"` | Receiver secret name |

### Audit bucket

| Variable | Type | Default | Description |
|---|---|---|---|
| `audit_bucket` | `string` | `""` | Existing S3 bucket for audit logs. Mutually exclusive with `create_audit_bucket` |
| `create_audit_bucket` | `bool` | `false` | Create a managed audit bucket with WORM compliance |
| `audit_bucket_retention_days` | `number` | `1095` | Object Lock retention (compliance mode). 1095 = 3 years |
| `audit_bucket_kms_key_arn` | `string` | `""` | CMK for audit bucket encryption (empty = SSE-S3) |
| `create_audit_access_log_bucket` | `bool` | `false` | Create a bucket for audit access logs. Mutually exclusive with `audit_access_log_bucket` |
| `audit_access_log_bucket` | `string` | `""` | Existing bucket for audit access logs |

### SQS / ECR events

| Variable | Type | Default | Description |
|---|---|---|---|
| `ecr_events_enabled` | `bool` | `false` | Create SQS queue and EventBridge rule for ECR push events |
| `ecr_events_visibility_timeout` | `number` | `300` | SQS visibility timeout in seconds |
| `sqs_kms_key_arn` | `string` | `""` | CMK for SQS encryption (empty = SSE-SQS) |

### ElastiCache IAM auth

| Variable | Type | Default | Description |
|---|---|---|---|
| `elasticache_replication_group_arn` | `string` | `""` | ElastiCache replication group ARN (empty = skip) |
| `elasticache_user_arn` | `string` | `""` | ElastiCache user ARN (required when replication group ARN is set) |

## Outputs

| Output | Description |
|---|---|
| `bot_role_arn` | Bot IAM role ARN (empty if identity_type is "user") |
| `receiver_role_arn` | Receiver IAM role ARN (empty if identity_type is "user") |
| `bot_user_name` | Bot IAM user name (empty if identity_type is "role") |
| `bot_user_arn` | Bot IAM user ARN (empty if identity_type is "role") |
| `receiver_user_name` | Receiver IAM user name (empty if identity_type is "role") |
| `receiver_user_arn` | Receiver IAM user ARN (empty if identity_type is "role") |
| `bot_policy_arn` | Bot managed policy ARN |
| `receiver_policy_arn` | Receiver managed policy ARN |
| `bot_secret_arn` | Bot Secrets Manager ARN (empty if create_secrets is false) |
| `receiver_secret_arn` | Receiver Secrets Manager ARN (empty if create_secrets is false) |
| `audit_bucket_name` | Audit bucket name (empty if no audit bucket configured) |
| `audit_bucket_arn` | Audit bucket ARN (empty if create_audit_bucket is false) |
| `sqs_queue_url` | SQS queue URL (empty if ECR events disabled) |
| `sqs_queue_arn` | SQS queue ARN (empty if ECR events disabled) |

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
  "ecr_auto_deploy": {
    "enabled": true,
    "sqs_queue_url": "<module.deploy_bot.sqs_queue_url>"
  }
}
```

The SQS visibility timeout defaults to 300 seconds (5 minutes) to allow the buffer retry window to complete before SQS redelivers. Adjust with `ecr_events_visibility_timeout` if needed.

### With ElastiCache IAM auth

Grant `elasticache:Connect` to both bot and receiver by providing the replication group and user ARNs. The application handles token generation — see [configuration.md](../docs/configuration.md#option-c-aws-secrets-manager-with-elasticache-iam-auth) for the secret format.

```hcl
module "deploy_bot" {
  source = "github.com/ezubriski/deploy-bot//terraform"

  region     = "us-east-1"
  account_id = "123456789012"

  eks_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
  eks_oidc_provider_url = "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"

  elasticache_replication_group_arn = "arn:aws:elasticache:us-east-1:123456789012:replicationgroup:deploy-bot"
  elasticache_user_arn             = "arn:aws:elasticache:us-east-1:123456789012:user:deploy-bot-iam"

  tags = {
    Team = "platform"
  }
}
```

A reference ElastiCache module with IAM auth, encryption in transit/at rest, and automatic failover is available in [`examples/elasticache/`](examples/elasticache/). It is provided as an example only and is not actively maintained — copy it into your own infrastructure and adapt as needed.

### With managed audit bucket

Create a WORM-compliant audit bucket with 3-year retention, Glacier lifecycle, and access logging:

```hcl
module "deploy_bot" {
  source = "github.com/ezubriski/deploy-bot//terraform"

  region     = "us-east-1"
  account_id = "123456789012"

  eks_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
  eks_oidc_provider_url = "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"

  create_audit_bucket             = true
  audit_bucket_retention_days     = 1095  # 3 years (SOC 2 / ISO 27001)
  create_audit_access_log_bucket  = true

  tags = {
    Team = "platform"
  }
}
```

The audit bucket enforces:
- **WORM** — Object Lock with compliance mode retention (objects cannot be deleted or overwritten during the retention period)
- **Encryption** — SSE-S3 by default, or a CMK via `audit_bucket_kms_key_arn`
- **Secure transport** — bucket policy denies all non-TLS requests
- **Deletion resistance** — `prevent_destroy` lifecycle, bucket policy denying `DeleteBucket`/`DeleteBucketPolicy`
- **Glacier lifecycle** — objects transition to Glacier after 90 days
- **Access logging** — all access to the audit bucket is logged (to a created or existing logging bucket)

To use an existing access logging bucket instead of creating one:

```hcl
  create_audit_access_log_bucket = false
  audit_access_log_bucket        = "my-shared-access-logs"
```

Logs are prefixed with the audit bucket name so multiple buckets can share one logging destination.

### Full example

```hcl
module "deploy_bot" {
  source = "github.com/ezubriski/deploy-bot//terraform"

  region     = "us-east-1"
  account_id = "123456789012"

  # IRSA
  eks_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
  eks_oidc_provider_url = "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"

  # Secrets (created by default, populate after apply)
  secrets_kms_key_arn = "arn:aws:kms:us-east-1:123456789012:key/example-key-id"

  # Audit
  create_audit_bucket            = true
  audit_bucket_kms_key_arn       = "arn:aws:kms:us-east-1:123456789012:key/example-key-id"
  create_audit_access_log_bucket = true

  # ECR events
  ecr_events_enabled = true
  sqs_kms_key_arn    = "arn:aws:kms:us-east-1:123456789012:key/example-key-id"

  # ElastiCache IAM auth
  elasticache_replication_group_arn = "arn:aws:elasticache:us-east-1:123456789012:replicationgroup:deploy-bot"
  elasticache_user_arn             = "arn:aws:elasticache:us-east-1:123456789012:user:deploy-bot-iam"

  tags = {
    Team = "platform"
  }
}
```
