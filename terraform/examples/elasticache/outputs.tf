output "replication_group_arn" {
  description = "ARN of the replication group. Pass to the deploy-bot module as elasticache_replication_group_arn"
  value       = aws_elasticache_replication_group.this.arn
}

output "iam_user_arn" {
  description = "ARN of the IAM-authenticated ElastiCache user. Pass to the deploy-bot module as elasticache_user_arn"
  value       = aws_elasticache_user.iam.arn
}

output "primary_endpoint" {
  description = "Primary endpoint address for the replication group"
  value       = aws_elasticache_replication_group.this.primary_endpoint_address
}

output "kms_key_arn" {
  description = "KMS key ARN used for at-rest encryption"
  value       = local.kms_key_arn
}
