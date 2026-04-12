variable "name" {
  description = "Name for the replication group and related resources"
  type        = string
  default     = "deploy-bot"
}

variable "subnet_ids" {
  description = "Subnet IDs for the ElastiCache subnet group"
  type        = list(string)
}

variable "security_group_ids" {
  description = "Security group IDs to associate with the replication group"
  type        = list(string)
}

variable "node_type" {
  description = "ElastiCache node type"
  type        = string
  default     = "cache.t4g.micro"
}

variable "num_cache_clusters" {
  description = "Number of cache clusters (nodes). Set to 2+ for automatic failover and multi-AZ"
  type        = number
  default     = 2
}

variable "engine_version" {
  description = "Redis engine version"
  type        = string
  default     = "7.1"
}

variable "parameter_group_name" {
  description = "ElastiCache parameter group name"
  type        = string
  default     = "default.redis7"
}

variable "kms_key_arn" {
  description = "ARN of a customer-managed KMS key for at-rest encryption. When empty (default), a key is created"
  type        = string
  default     = ""
}

variable "default_user_password" {
  description = "Password for the disabled default user (required by ElastiCache). Use a long random value — this user has no command access"
  type        = string
  sensitive   = true
}

variable "maintenance_window" {
  description = "Weekly maintenance window"
  type        = string
  default     = "sun:05:00-sun:06:00"
}

variable "snapshot_retention_days" {
  description = "Number of days to retain automatic snapshots. 0 disables snapshots. Default 0: Redis holds only ephemeral data (streams, locks, caches) since 3.0 — all durable state is in Postgres, so snapshots provide no recovery value. Set > 0 only if your ops team wants them for debugging."
  type        = number
  default     = 0
}

variable "snapshot_window" {
  description = "Daily window for automatic snapshots"
  type        = string
  default     = "03:00-04:00"
}

variable "tags" {
  description = "Tags to apply to all resources"
  type        = map(string)
  default     = {}
}
