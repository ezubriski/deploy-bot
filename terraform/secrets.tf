# Optional Secrets Manager secrets for bot and receiver components.
# Created when create_secrets is true. Secrets are created empty —
# populate them after apply.

locals {
  secrets_cmk = var.secrets_kms_key_arn != ""
}

resource "aws_secretsmanager_secret" "bot" {
  count = var.create_secrets ? 1 : 0

  name        = var.bot_secrets_manager_secret_name
  description = "Bot (worker) component secrets for ${var.name}"
  kms_key_id  = local.secrets_cmk ? var.secrets_kms_key_arn : null
  tags        = var.tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_secretsmanager_secret" "receiver" {
  count = var.create_secrets ? 1 : 0

  name        = var.receiver_secrets_manager_secret_name
  description = "Receiver component secrets for ${var.name}"
  kms_key_id  = local.secrets_cmk ? var.secrets_kms_key_arn : null
  tags        = var.tags

  lifecycle {
    prevent_destroy = true
  }
}
