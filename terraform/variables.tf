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
  description = "ARN of the EKS OIDC provider for IRSA"
  type        = string
}

variable "eks_oidc_provider_url" {
  description = "URL of the EKS OIDC provider (without https://)"
  type        = string
}

variable "namespace" {
  description = "Kubernetes namespace where the bot runs"
  type        = string
  default     = "deploy-bot"
}

variable "service_account_name" {
  description = "Kubernetes ServiceAccount name"
  type        = string
  default     = "deploy-bot"
}

variable "secrets_manager_secret_name" {
  description = "Name of the Secrets Manager secret containing bot tokens"
  type        = string
  default     = "deploy-bot/secrets"
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

variable "tags" {
  description = "Tags to apply to all resources"
  type        = map(string)
  default     = {}
}
