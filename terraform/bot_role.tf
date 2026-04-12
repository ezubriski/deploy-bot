# IAM role for the bot (worker) component.
# Needs: Secrets Manager (bot secret), ECR read, S3 audit write.
# Only created when at least one trust source is configured (IRSA or EC2).

data "aws_iam_policy_document" "bot_assume_role" {
  count = local.create_roles ? 1 : 0

  dynamic "statement" {
    for_each = local.create_irsa_roles ? [1] : []
    content {
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

  dynamic "statement" {
    for_each = var.enable_ec2_trust ? [1] : []
    content {
      actions = ["sts:AssumeRole"]
      effect  = "Allow"

      principals {
        type        = "Service"
        identifiers = ["ec2.amazonaws.com"]
      }
    }
  }
}

resource "aws_iam_role" "bot" {
  count = local.create_roles ? 1 : 0

  name                 = "${var.name}-bot"
  assume_role_policy   = data.aws_iam_policy_document.bot_assume_role[0].json
  permissions_boundary = var.permissions_boundary != "" ? var.permissions_boundary : null
  tags                 = var.tags
}

# --- Bot managed policy: Secrets Manager + ECR + S3 audit ---

data "aws_iam_policy_document" "bot" {
  statement {
    sid     = "ReadBotSecret"
    actions = ["secretsmanager:GetSecretValue"]
    resources = [
      "arn:aws:secretsmanager:${var.region}:${var.account_id}:secret:${var.bot_secrets_manager_secret_name}-*"
    ]
  }

  dynamic "statement" {
    for_each = local.secrets_cmk ? [1] : []
    content {
      sid       = "DecryptBotSecret"
      actions   = ["kms:Decrypt"]
      resources = [var.secrets_kms_key_arn]
    }
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
    for_each = local.audit_bucket_enabled ? [1] : []
    content {
      sid       = "WriteAuditLog"
      actions   = ["s3:PutObject"]
      resources = ["arn:aws:s3:::${local.audit_bucket_name}/${var.name}/*"]
    }
  }

  dynamic "statement" {
    for_each = var.create_audit_bucket && local.audit_bucket_cmk ? [1] : []
    content {
      sid = "EncryptAuditLog"
      actions = [
        "kms:GenerateDataKey",
        "kms:Decrypt",
      ]
      resources = [var.audit_bucket_kms_key_arn]
    }
  }

  dynamic "statement" {
    for_each = local.elasticache_iam ? [1] : []
    content {
      sid     = "ElastiCacheIAMAuth"
      actions = ["elasticache:Connect"]
      resources = [
        var.elasticache_replication_group_arn,
        var.elasticache_user_arn,
      ]
    }
  }

  dynamic "statement" {
    for_each = local.rds_iam ? [1] : []
    content {
      sid       = "RDSIAMAuth"
      actions   = ["rds-db:connect"]
      resources = [local.rds_connect_arn]
    }
  }
}

resource "aws_iam_policy" "bot" {
  name   = "${var.name}-bot"
  policy = data.aws_iam_policy_document.bot.json
  tags   = var.tags
}

resource "aws_iam_role_policy_attachment" "bot" {
  count = local.create_roles ? 1 : 0

  role       = aws_iam_role.bot[0].name
  policy_arn = aws_iam_policy.bot.arn
}
