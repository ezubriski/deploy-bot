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
}

# --- SQS queue for ECR push events (optional, shared infra) ---

resource "aws_sqs_queue" "ecr_events" {
  count = var.ecr_events_enabled ? 1 : 0

  name                       = "${var.name}-ecr-events"
  visibility_timeout_seconds = var.ecr_events_visibility_timeout
  message_retention_seconds  = 86400
  tags                       = var.tags
}

# Allow EventBridge to send messages to the queue.
data "aws_iam_policy_document" "sqs_policy" {
  count = var.ecr_events_enabled ? 1 : 0

  statement {
    sid     = "AllowEventBridge"
    actions = ["sqs:SendMessage"]
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
