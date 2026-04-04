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
  description = "S3 bucket for audit logs. Leave empty to disable S3 audit logging"
  type        = string
  default     = ""
}

variable "ecr_events_enabled" {
  description = "Enable EventBridge ECR push event pipeline (SQS queue + EventBridge rule)"
  type        = bool
  default     = false
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

variable "tags" {
  description = "Tags to apply to all resources"
  type        = map(string)
  default     = {}
}
