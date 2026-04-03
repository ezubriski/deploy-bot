output "bot_role_arn" {
  description = "IAM role ARN for the bot (worker) ServiceAccount annotation"
  value       = aws_iam_role.bot.arn
}

output "receiver_role_arn" {
  description = "IAM role ARN for the receiver ServiceAccount annotation"
  value       = aws_iam_role.receiver.arn
}

output "sqs_queue_url" {
  description = "SQS queue URL for ecr_events.sqs_queue_url config (empty if ECR events disabled)"
  value       = var.ecr_events_enabled ? aws_sqs_queue.ecr_events[0].url : ""
}

output "sqs_queue_arn" {
  description = "SQS queue ARN (empty if ECR events disabled)"
  value       = var.ecr_events_enabled ? aws_sqs_queue.ecr_events[0].arn : ""
}
