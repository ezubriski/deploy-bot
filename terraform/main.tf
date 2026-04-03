# IAM role for the deploy-bot Kubernetes ServiceAccount (IRSA).
# Grants access to Secrets Manager, ECR (all repos), S3 audit, and
# optionally SQS for ECR push events.

data "aws_iam_policy_document" "assume_role" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    effect  = "Allow"

    principals {
      type        = "Federated"
      identifiers = [var.eks_oidc_provider_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.eks_oidc_provider_url}:sub"
      values   = ["system:serviceaccount:${var.namespace}:${var.service_account_name}"]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.eks_oidc_provider_url}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "bot" {
  name                 = var.name
  assume_role_policy   = data.aws_iam_policy_document.assume_role.json
  permissions_boundary = var.permissions_boundary != "" ? var.permissions_boundary : null
  tags                 = var.tags
}

# --- Secrets Manager ---

data "aws_iam_policy_document" "secrets" {
  statement {
    sid     = "ReadBotSecret"
    actions = ["secretsmanager:GetSecretValue"]
    resources = [
      "arn:aws:secretsmanager:${var.region}:${var.account_id}:secret:${var.secrets_manager_secret_name}-*"
    ]
  }
}

resource "aws_iam_role_policy" "secrets" {
  name   = "${var.name}-secrets"
  role   = aws_iam_role.bot.id
  policy = data.aws_iam_policy_document.secrets.json
}

# --- ECR (all repositories) ---
# The bot needs to list tags and pull image metadata for any app it manages.
# ecr:GetAuthorizationToken requires Resource: "*" and cannot be scoped.

data "aws_iam_policy_document" "ecr" {
  statement {
    sid = "ReadECR"
    actions = [
      "ecr:GetAuthorizationToken",
      "ecr:DescribeImages",
      "ecr:BatchGetImage",
      "ecr:ListImages",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "ecr" {
  name   = "${var.name}-ecr"
  role   = aws_iam_role.bot.id
  policy = data.aws_iam_policy_document.ecr.json
}

# --- S3 audit log (optional) ---

data "aws_iam_policy_document" "audit" {
  count = var.audit_bucket != "" ? 1 : 0

  statement {
    sid       = "WriteAuditLog"
    actions   = ["s3:PutObject"]
    resources = ["arn:aws:s3:::${var.audit_bucket}/${var.name}/*"]
  }
}

resource "aws_iam_role_policy" "audit" {
  count  = var.audit_bucket != "" ? 1 : 0
  name   = "${var.name}-audit"
  role   = aws_iam_role.bot.id
  policy = data.aws_iam_policy_document.audit[0].json
}

# --- SQS for ECR push events (optional) ---

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

# Grant the bot permission to receive and delete from the queue.
data "aws_iam_policy_document" "sqs" {
  count = var.ecr_events_enabled ? 1 : 0

  statement {
    sid = "ReceiveECREvents"
    actions = [
      "sqs:ReceiveMessage",
      "sqs:DeleteMessage",
      "sqs:GetQueueAttributes",
    ]
    resources = [aws_sqs_queue.ecr_events[0].arn]
  }
}

resource "aws_iam_role_policy" "sqs" {
  count  = var.ecr_events_enabled ? 1 : 0
  name   = "${var.name}-sqs"
  role   = aws_iam_role.bot.id
  policy = data.aws_iam_policy_document.sqs[0].json
}

# --- EventBridge rule for ECR push events ---
# Captures all successful ECR image pushes across the account.
# The bot's poller filters by repository name and tag pattern.

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
