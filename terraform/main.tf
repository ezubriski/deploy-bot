terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

locals {
  create_irsa_roles = var.eks_oidc_provider_arn != "" && var.eks_oidc_provider_url != ""
  create_roles      = var.identity_type == "role" && (local.create_irsa_roles || var.enable_ec2_trust)
  create_users      = var.identity_type == "user"
  elasticache_iam   = var.elasticache_replication_group_arn != ""
}

check "elasticache_user_required" {
  assert {
    condition     = !(local.elasticache_iam && var.elasticache_user_arn == "")
    error_message = "elasticache_user_arn is required when elasticache_replication_group_arn is set."
  }
}

check "audit_bucket_mutual_exclusion" {
  assert {
    condition     = !(var.create_audit_bucket && var.audit_bucket != "")
    error_message = "audit_bucket and create_audit_bucket are mutually exclusive. Set one or the other, not both."
  }
}

# --- SQS queue for ECR push events (optional, shared infra) ---

locals {
  sqs_use_cmk = var.sqs_kms_key_arn != ""
}

resource "aws_sqs_queue" "ecr_events" {
  count = var.ecr_events_enabled ? 1 : 0

  name                       = "${var.name}-ecr-events"
  visibility_timeout_seconds = var.ecr_events_visibility_timeout
  message_retention_seconds  = 86400

  sqs_managed_sse_enabled = local.sqs_use_cmk ? null : true
  kms_master_key_id       = local.sqs_use_cmk ? var.sqs_kms_key_arn : null

  tags = var.tags
}

# Allow EventBridge to send messages to the queue.
data "aws_iam_policy_document" "sqs_policy" {
  count = var.ecr_events_enabled ? 1 : 0

  statement {
    sid       = "AllowEventBridge"
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.ecr_events[0].arn]

    principals {
      type        = "Service"
      identifiers = ["events.amazonaws.com"]
    }

    condition {
      test     = "ArnEquals"
      variable = "aws:SourceArn"
      values   = [aws_cloudwatch_event_rule.ecr_push[0].arn]
    }
  }

  dynamic "statement" {
    for_each = local.sqs_use_cmk ? [1] : []
    content {
      sid = "AllowEventBridgeKMS"
      actions = [
        "kms:Decrypt",
        "kms:GenerateDataKey",
      ]
      resources = [var.sqs_kms_key_arn]

      principals {
        type        = "Service"
        identifiers = ["events.amazonaws.com"]
      }
    }
  }
}

resource "aws_sqs_queue_policy" "ecr_events" {
  count     = var.ecr_events_enabled ? 1 : 0
  queue_url = aws_sqs_queue.ecr_events[0].id
  policy    = data.aws_iam_policy_document.sqs_policy[0].json
}

# --- EventBridge rule for ECR push events ---

resource "aws_cloudwatch_event_rule" "ecr_push" {
  count = var.ecr_events_enabled ? 1 : 0

  name        = "${var.name}-ecr-push"
  description = "Capture ECR image push events for deploy-bot"
  tags        = var.tags

  event_pattern = jsonencode({
    source      = ["aws.ecr"]
    detail-type = ["ECR Image Action"]
    detail = {
      action-type = ["PUSH"]
      result      = ["SUCCESS"]
    }
  })
}

resource "aws_cloudwatch_event_target" "ecr_push_sqs" {
  count = var.ecr_events_enabled ? 1 : 0

  rule      = aws_cloudwatch_event_rule.ecr_push[0].name
  target_id = "${var.name}-sqs"
  arn       = aws_sqs_queue.ecr_events[0].arn
}
