# IAM role for the receiver component.
# Needs: Secrets Manager, SQS read (when ECR events enabled).
# Only created when IRSA (EKS OIDC) variables are provided.

data "aws_iam_policy_document" "receiver_assume_role" {
  count = local.create_irsa_roles ? 1 : 0

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
      values   = ["system:serviceaccount:${var.namespace}:${var.receiver_service_account_name}"]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.eks_oidc_provider_url}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "receiver" {
  count = local.create_irsa_roles ? 1 : 0

  name                 = "${var.name}-receiver"
  assume_role_policy   = data.aws_iam_policy_document.receiver_assume_role[0].json
  permissions_boundary = var.permissions_boundary != "" ? var.permissions_boundary : null
  tags                 = var.tags
}

# --- Receiver managed policy: Secrets Manager + SQS ---

data "aws_iam_policy_document" "receiver" {
  statement {
    sid     = "ReadBotSecret"
    actions = ["secretsmanager:GetSecretValue"]
    resources = [
      "arn:aws:secretsmanager:${var.region}:${var.account_id}:secret:${var.secrets_manager_secret_name}-*"
    ]
  }

  dynamic "statement" {
    for_each = var.ecr_events_enabled ? [1] : []
    content {
      sid = "ReceiveECREvents"
      actions = [
        "sqs:ReceiveMessage",
        "sqs:DeleteMessage",
        "sqs:GetQueueAttributes",
      ]
      resources = [aws_sqs_queue.ecr_events[0].arn]
    }
  }
}

resource "aws_iam_policy" "receiver" {
  name   = "${var.name}-receiver"
  policy = data.aws_iam_policy_document.receiver.json
  tags   = var.tags
}

resource "aws_iam_role_policy_attachment" "receiver" {
  count = local.create_irsa_roles ? 1 : 0

  role       = aws_iam_role.receiver[0].name
  policy_arn = aws_iam_policy.receiver.arn
}
