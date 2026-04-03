# deploy-bot Terraform module

Creates the AWS resources needed to run deploy-bot:

- **IAM role** with IRSA trust policy for the Kubernetes ServiceAccount
- **IAM policies** for Secrets Manager, ECR (all repositories), and optionally S3 audit logging
- **SQS queue + EventBridge rule** (optional) for ECR push-triggered deploys

## Usage

```hcl
module "deploy_bot" {
  source = "./terraform"

  region                = "us-east-1"
  account_id            = "123456789012"
  eks_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
  eks_oidc_provider_url = "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
  audit_bucket          = "my-audit-logs"
  ecr_events_enabled    = true

  tags = {
    Team = "platform"
  }
}
```

## ECR events

When `ecr_events_enabled = true`, the module creates:

1. An SQS queue (`deploy-bot-ecr-events`)
2. An EventBridge rule that captures all successful ECR image pushes in the account
3. An EventBridge target that sends matched events to the SQS queue
4. A queue policy allowing EventBridge to send messages
5. An IAM policy granting the bot `sqs:ReceiveMessage` and `sqs:DeleteMessage`

The EventBridge rule captures pushes across **all repositories** in the account. The bot's poller filters events by matching `repository-name` against configured apps and validating tags against `tag_pattern`. This means no EventBridge changes are needed when adding new apps.

Set the `sqs_queue_url` output in your `config.json`:

```json
{
  "ecr_events": {
    "sqs_queue_url": "<module.deploy_bot.sqs_queue_url>"
  }
}
```

The SQS visibility timeout defaults to 300 seconds (5 minutes) to allow the buffer retry window to complete before SQS redelivers. Adjust with `ecr_events_visibility_timeout` if needed.

## Outputs

| Output | Description |
|---|---|
| `role_arn` | IAM role ARN -- set in the Kubernetes ServiceAccount `eks.amazonaws.com/role-arn` annotation |
| `sqs_queue_url` | SQS queue URL -- set in `ecr_events.sqs_queue_url` config |
| `sqs_queue_arn` | SQS queue ARN |
