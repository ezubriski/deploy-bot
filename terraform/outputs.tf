output "role_arn" {
  description = "IAM role ARN to set in the ServiceAccount annotation"
  value       = aws_iam_role.bot.arn
}

output "sqs_queue_url" {
  description = "SQS queue URL for ecr_events.sqs_queue_url config (empty if ECR events disabled)"
  value       = var.ecr_events_enabled ? aws_sqs_queue.ecr_events[0].url : ""
}

output "sqs_queue_arn" {
  description = "SQS queue ARN (empty if ECR events disabled)"
  value       = var.ecr_events_enabled ? aws_sqs_queue.ecr_events[0].arn : ""
}
