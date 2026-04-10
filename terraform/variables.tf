variable "name" {
  description = "Name prefix for all resources"
  type        = string
  default     = "deploy-bot"
}

variable "region" {
  description = "AWS region"
  type        = string
}

variable "account_id" {
  description = "AWS account ID where the bot runs"
  type        = string
}

variable "eks_oidc_provider_arn" {
  description = "ARN of the EKS OIDC provider for IRSA. Leave empty to skip IAM role creation"
  type        = string
  default     = ""
}

variable "eks_oidc_provider_url" {
  description = "URL of the EKS OIDC provider (without https://). Leave empty to skip IAM role creation"
  type        = string
  default     = ""
}

variable "namespace" {
  description = "Kubernetes namespace where the bot runs"
  type        = string
  default     = "deploy-bot"
}

variable "bot_service_account_name" {
  description = "Kubernetes ServiceAccount name for the bot (worker) component"
  type        = string
  default     = "deploy-bot-worker"
}

variable "receiver_service_account_name" {
  description = "Kubernetes ServiceAccount name for the receiver component"
  type        = string
  default     = "deploy-bot-receiver"
}

variable "create_secrets" {
  description = "Create Secrets Manager secrets for the bot and receiver components. Secrets are created empty — populate them after apply"
  type        = bool
  default     = true
}

variable "secrets_kms_key_arn" {
  description = "ARN of a customer-managed KMS key for Secrets Manager encryption. When empty (default), the AWS managed key (aws/secretsmanager) is used. Only applies when create_secrets is true"
  type        = string
  default     = ""
}

variable "bot_secrets_manager_secret_name" {
  description = "Name of the Secrets Manager secret for the bot (worker) component"
  type        = string
  default     = "deploy-bot/bot-secrets"
}

variable "receiver_secrets_manager_secret_name" {
  description = "Name of the Secrets Manager secret for the receiver component"
  type        = string
  default     = "deploy-bot/receiver-secrets"
}

variable "audit_bucket" {
  description = "Name of an existing S3 bucket for audit logs. Mutually exclusive with create_audit_bucket. Leave empty to disable S3 audit logging"
  type        = string
  default     = ""
}

variable "create_audit_bucket" {
  description = "Create a managed S3 audit bucket with WORM compliance, encryption, and secure transport enforcement"
  type        = bool
  default     = false
}

variable "audit_bucket_retention_days" {
  description = "Object Lock retention period in days (compliance mode). Only applies when create_audit_bucket is true"
  type        = number
  default     = 1095
}

variable "audit_bucket_kms_key_arn" {
  description = "ARN of a customer-managed KMS key for audit bucket encryption. When empty (default), SSE-S3 is used. Only applies when create_audit_bucket is true"
  type        = string
  default     = ""
}

variable "create_audit_access_log_bucket" {
  description = "Create a managed S3 bucket for audit bucket access logs. Mutually exclusive with audit_access_log_bucket. Only applies when create_audit_bucket is true"
  type        = bool
  default     = false
}

variable "audit_access_log_bucket" {
  description = "Name of an existing S3 bucket for audit bucket access logs. Mutually exclusive with create_audit_access_log_bucket. Only applies when create_audit_bucket is true"
  type        = string
  default     = ""
}

variable "elasticache_replication_group_arn" {
  description = "ARN of the ElastiCache replication group for IAM auth. Leave empty to skip ElastiCache IAM permissions"
  type        = string
  default     = ""
}

variable "elasticache_user_arn" {
  description = "ARN of the ElastiCache user for IAM auth. Required when elasticache_replication_group_arn is set"
  type        = string
  default     = ""
}

variable "ecr_events_enabled" {
  description = "Enable EventBridge ECR push event pipeline (SQS queue + EventBridge rule)"
  type        = bool
  default     = false
}

variable "ecr_webhook_enabled" {
  description = "Enable EventBridge API Destination for HTTP webhook delivery of ECR push events"
  type        = bool
  default     = false
}

variable "ecr_webhook_endpoint" {
  description = "HTTPS endpoint URL for the ECR webhook (e.g. https://deploy-bot.example.com/v1/webhooks/ecr)"
  type        = string
  default     = ""
}

variable "ecr_webhook_api_key" {
  description = "API key for authenticating to the webhook endpoint. Must be at least 32 characters"
  type        = string
  default     = ""
  sensitive   = true
}

variable "ecr_events_visibility_timeout" {
  description = "SQS visibility timeout in seconds. Should exceed the buffer retry window"
  type        = number
  default     = 300
}

variable "identity_type" {
  description = "Type of IAM identity to create: \"role\" (default, requires trust source) or \"user\" (no trust policy needed)"
  type        = string
  default     = "role"

  validation {
    condition     = contains(["role", "user"], var.identity_type)
    error_message = "identity_type must be \"role\" or \"user\""
  }
}

variable "enable_ec2_trust" {
  description = "Add an EC2 trust policy to the roles, allowing EC2 instances to assume them. Only applies when identity_type is \"role\""
  type        = bool
  default     = false
}

variable "permissions_boundary" {
  description = "ARN of an IAM permissions boundary to attach to the bot role. Leave empty for no boundary"
  type        = string
  default     = ""
}

variable "sqs_kms_key_arn" {
  description = "ARN of a customer-managed KMS key for SQS encryption. When empty (default), SQS-managed SSE (SSE-SQS) is used"
  type        = string
  default     = ""
}

variable "tags" {
  description = "Tags to apply to all resources"
  type        = map(string)
  default     = {}
}
