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
  rds_iam           = var.rds_resource_id != ""
  rds_connect_arn   = local.rds_iam ? "arn:aws:rds-db:${var.region}:${var.account_id}:dbuser:${var.rds_resource_id}/${var.rds_db_user}" : ""
}

check "rds_db_user_required" {
  assert {
    condition     = !(local.rds_iam && var.rds_db_user == "")
    error_message = "rds_db_user is required when rds_resource_id is set."
  }
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
  count = (var.ecr_events_enabled || var.ecr_webhook_enabled) ? 1 : 0

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

# --- EventBridge API Destination for HTTP webhook delivery (optional) ---

resource "aws_cloudwatch_event_connection" "ecr_webhook" {
  count = var.ecr_webhook_enabled ? 1 : 0

  name               = "${var.name}-ecr-webhook"
  description        = "API key auth for deploy-bot ECR webhook"
  authorization_type = "API_KEY"

  auth_parameters {
    api_key {
      key   = "x-api-key"
      value = var.ecr_webhook_api_key
    }
  }
}

resource "aws_cloudwatch_event_api_destination" "ecr_webhook" {
  count = var.ecr_webhook_enabled ? 1 : 0

  name                             = "${var.name}-ecr-webhook"
  description                      = "HTTP endpoint for ECR push events"
  invocation_endpoint              = var.ecr_webhook_endpoint
  http_method                      = "POST"
  invocation_rate_limit_per_second = 10
  connection_arn                   = aws_cloudwatch_event_connection.ecr_webhook[0].arn
}

resource "aws_cloudwatch_event_target" "ecr_push_webhook" {
  count = var.ecr_webhook_enabled ? 1 : 0

  rule      = aws_cloudwatch_event_rule.ecr_push[0].name
  target_id = "${var.name}-webhook"
  arn       = aws_cloudwatch_event_api_destination.ecr_webhook[0].arn
  role_arn  = aws_iam_role.eventbridge_webhook[0].arn
}

data "aws_iam_policy_document" "eventbridge_webhook_assume" {
  count = var.ecr_webhook_enabled ? 1 : 0

  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["events.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "eventbridge_webhook" {
  count = var.ecr_webhook_enabled ? 1 : 0

  name               = "${var.name}-eb-webhook"
  assume_role_policy = data.aws_iam_policy_document.eventbridge_webhook_assume[0].json
  tags               = var.tags
}

data "aws_iam_policy_document" "eventbridge_webhook" {
  count = var.ecr_webhook_enabled ? 1 : 0

  statement {
    sid       = "InvokeAPIDestination"
    actions   = ["events:InvokeApiDestination"]
    resources = [aws_cloudwatch_event_api_destination.ecr_webhook[0].arn]
  }
}

resource "aws_iam_policy" "eventbridge_webhook" {
  count = var.ecr_webhook_enabled ? 1 : 0

  name   = "${var.name}-eb-webhook"
  policy = data.aws_iam_policy_document.eventbridge_webhook[0].json
  tags   = var.tags
}

resource "aws_iam_role_policy_attachment" "eventbridge_webhook" {
  count = var.ecr_webhook_enabled ? 1 : 0

  role       = aws_iam_role.eventbridge_webhook[0].name
  policy_arn = aws_iam_policy.eventbridge_webhook[0].arn
}
