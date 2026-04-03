# IAM role for the bot (worker) component.
# Needs: Secrets Manager, ECR read, S3 audit write.

data "aws_iam_policy_document" "bot_assume_role" {
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
      values   = ["system:serviceaccount:${var.namespace}:${var.bot_service_account_name}"]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.eks_oidc_provider_url}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "bot" {
  name                 = "${var.name}-bot"
  assume_role_policy   = data.aws_iam_policy_document.bot_assume_role.json
  permissions_boundary = var.permissions_boundary != "" ? var.permissions_boundary : null
  tags                 = var.tags
}

# --- Bot managed policy: Secrets Manager + ECR + S3 audit ---

data "aws_iam_policy_document" "bot" {
  statement {
    sid     = "ReadBotSecret"
    actions = ["secretsmanager:GetSecretValue"]
    resources = [
      "arn:aws:secretsmanager:${var.region}:${var.account_id}:secret:${var.secrets_manager_secret_name}-*"
    ]
  }

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

  dynamic "statement" {
    for_each = var.audit_bucket != "" ? [1] : []
    content {
      sid       = "WriteAuditLog"
      actions   = ["s3:PutObject"]
      resources = ["arn:aws:s3:::${var.audit_bucket}/${var.name}/*"]
    }
  }
}

resource "aws_iam_policy" "bot" {
  name   = "${var.name}-bot"
  policy = data.aws_iam_policy_document.bot.json
  tags   = var.tags
}

resource "aws_iam_role_policy_attachment" "bot" {
  role       = aws_iam_role.bot.name
  policy_arn = aws_iam_policy.bot.arn
}
