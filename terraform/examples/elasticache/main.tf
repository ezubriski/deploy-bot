# ElastiCache Redis example for deploy-bot.
#
# This is a reference implementation showing recommended settings for
# ElastiCache Redis with IAM authentication, encryption, and automatic
# failover. It is provided as an example only and is not actively
# maintained. Copy it into your own infrastructure repository and adapt
# as needed.
#
# 3.0+ note: Redis holds only ephemeral data (event streams, locks,
# caches, dedupe markers). All durable state is in Postgres. The
# settings here are tuned for AVAILABILITY (Multi-AZ, automatic
# failover) not DURABILITY (snapshots disabled by default, no AOF).
# A Redis restart or flush loses in-flight stream events — the bot's
# dedupe layer and Slack's at-least-once retries handle re-delivery.

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

# --- KMS key for at-rest encryption ---

resource "aws_kms_key" "redis" {
  count = var.kms_key_arn == "" ? 1 : 0

  description         = "ElastiCache encryption key for ${var.name}"
  enable_key_rotation = true
  tags                = var.tags
}

resource "aws_kms_alias" "redis" {
  count = var.kms_key_arn == "" ? 1 : 0

  name          = "alias/${var.name}-redis"
  target_key_id = aws_kms_key.redis[0].key_id
}

locals {
  kms_key_arn = var.kms_key_arn != "" ? var.kms_key_arn : aws_kms_key.redis[0].arn
}

# --- Subnet group ---

resource "aws_elasticache_subnet_group" "this" {
  name       = var.name
  subnet_ids = var.subnet_ids
  tags       = var.tags
}

# --- IAM auth user ---

resource "aws_elasticache_user" "iam" {
  user_id       = "${var.name}-iam"
  user_name     = "${var.name}-iam"
  access_string = "on ~* +@all"
  engine        = "REDIS"

  authentication_mode {
    type = "iam"
  }

  tags = var.tags
}

# Default user is required by ElastiCache but should not be used.
# Disable it with an impossible password and no command access.
resource "aws_elasticache_user" "default" {
  user_id       = "${var.name}-default"
  user_name     = "default"
  access_string = "off -@all"
  engine        = "REDIS"

  authentication_mode {
    type      = "password"
    passwords = [var.default_user_password]
  }

  tags = var.tags

  lifecycle {
    ignore_changes = [authentication_mode]
  }
}

resource "aws_elasticache_user_group" "this" {
  engine        = "REDIS"
  user_group_id = var.name
  user_ids      = [aws_elasticache_user.default.user_id, aws_elasticache_user.iam.user_id]
  tags          = var.tags
}

# --- Replication group ---

resource "aws_elasticache_replication_group" "this" {
  replication_group_id = var.name
  description          = "Redis for ${var.name}"
  engine               = "redis"
  engine_version       = var.engine_version
  node_type            = var.node_type
  num_cache_clusters   = var.num_cache_clusters
  port                 = 6379
  parameter_group_name = var.parameter_group_name

  # Networking
  subnet_group_name  = aws_elasticache_subnet_group.this.name
  security_group_ids = var.security_group_ids

  # Encryption in transit (TLS required)
  transit_encryption_enabled = true

  # Encryption at rest
  at_rest_encryption_enabled = true
  kms_key_id                 = local.kms_key_arn

  # IAM authentication
  user_group_ids = [aws_elasticache_user_group.this.user_group_id]

  # High availability
  automatic_failover_enabled = var.num_cache_clusters > 1
  multi_az_enabled           = var.num_cache_clusters > 1

  # Maintenance
  maintenance_window         = var.maintenance_window
  snapshot_retention_limit   = var.snapshot_retention_days
  snapshot_window            = var.snapshot_window
  auto_minor_version_upgrade = true
  apply_immediately          = false

  tags = var.tags
}
