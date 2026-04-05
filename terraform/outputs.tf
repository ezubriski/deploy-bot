output "bot_role_arn" {
  description = "IAM role ARN for the bot (worker) ServiceAccount annotation (empty if identity_type is \"user\" or no trust source configured)"
  value       = local.create_roles ? aws_iam_role.bot[0].arn : ""
}

output "receiver_role_arn" {
  description = "IAM role ARN for the receiver ServiceAccount annotation (empty if identity_type is \"user\" or no trust source configured)"
  value       = local.create_roles ? aws_iam_role.receiver[0].arn : ""
}

output "bot_user_name" {
  description = "IAM user name for the bot (worker) component (empty if identity_type is \"role\")"
  value       = local.create_users ? aws_iam_user.bot[0].name : ""
}

output "bot_user_arn" {
  description = "IAM user ARN for the bot (worker) component (empty if identity_type is \"role\")"
  value       = local.create_users ? aws_iam_user.bot[0].arn : ""
}

output "receiver_user_name" {
  description = "IAM user name for the receiver component (empty if identity_type is \"role\")"
  value       = local.create_users ? aws_iam_user.receiver[0].name : ""
}

output "receiver_user_arn" {
  description = "IAM user ARN for the receiver component (empty if identity_type is \"role\")"
  value       = local.create_users ? aws_iam_user.receiver[0].arn : ""
}

output "bot_policy_arn" {
  description = "IAM policy ARN for the bot (worker) component"
  value       = aws_iam_policy.bot.arn
}

output "receiver_policy_arn" {
  description = "IAM policy ARN for the receiver component"
  value       = aws_iam_policy.receiver.arn
}

output "bot_secret_arn" {
  description = "Secrets Manager ARN for the bot secret (empty if create_secrets is false)"
  value       = var.create_secrets ? aws_secretsmanager_secret.bot[0].arn : ""
}

output "receiver_secret_arn" {
  description = "Secrets Manager ARN for the receiver secret (empty if create_secrets is false)"
  value       = var.create_secrets ? aws_secretsmanager_secret.receiver[0].arn : ""
}

output "audit_bucket_name" {
  description = "S3 audit bucket name (empty if no audit bucket is configured)"
  value       = local.audit_bucket_enabled ? local.audit_bucket_name : ""
}

output "audit_bucket_arn" {
  description = "S3 audit bucket ARN (empty if create_audit_bucket is false)"
  value       = var.create_audit_bucket ? aws_s3_bucket.audit[0].arn : ""
}

output "sqs_queue_url" {
  description = "SQS queue URL for ecr_events.sqs_queue_url config (empty if ECR events disabled)"
  value       = var.ecr_events_enabled ? aws_sqs_queue.ecr_events[0].url : ""
}

output "sqs_queue_arn" {
  description = "SQS queue ARN (empty if ECR events disabled)"
  value       = var.ecr_events_enabled ? aws_sqs_queue.ecr_events[0].arn : ""
}
