# ElastiCache Redis Example

> **Note:** This is a reference implementation provided as an example only. It
> is not actively maintained as part of the deploy-bot module. Copy it into your
> own infrastructure repository and adapt it to your requirements.

This example creates an ElastiCache Redis replication group with the following
recommended settings:

- **IAM authentication** — no static passwords for application access
- **Encryption in transit** — TLS required for all connections
- **Encryption at rest** — customer-managed KMS key (created automatically or
  bring your own)
- **Automatic failover** — enabled when `num_cache_clusters >= 2`
- **Multi-AZ** — enabled when `num_cache_clusters >= 2`
- **Disabled default user** — required by ElastiCache but locked down with no
  command access
- **Automatic snapshots** — 7-day retention by default
- **Auto minor version upgrades** — enabled

## Usage

```hcl
module "redis" {
  source = "./path/to/this/example"

  name               = "deploy-bot"
  subnet_ids         = ["subnet-abc", "subnet-def"]
  security_group_ids = ["sg-123"]

  default_user_password = random_password.redis_default.result
}

module "deploy_bot" {
  source = "../../"

  # ... other variables ...

  elasticache_replication_group_arn = module.redis.replication_group_arn
  elasticache_user_arn             = module.redis.iam_user_arn
}
```

## Inputs

| Name | Description | Type | Default |
|------|-------------|------|---------|
| `name` | Name for resources | `string` | `"deploy-bot"` |
| `subnet_ids` | Subnet IDs for the subnet group | `list(string)` | — |
| `security_group_ids` | Security groups for the replication group | `list(string)` | — |
| `node_type` | ElastiCache node type | `string` | `"cache.t4g.micro"` |
| `num_cache_clusters` | Number of nodes (2+ for failover) | `number` | `2` |
| `engine_version` | Redis engine version | `string` | `"7.1"` |
| `parameter_group_name` | Parameter group name | `string` | `"default.redis7"` |
| `kms_key_arn` | KMS key ARN for at-rest encryption (empty = create one) | `string` | `""` |
| `default_user_password` | Password for the disabled default user | `string` | — |
| `maintenance_window` | Weekly maintenance window | `string` | `"sun:05:00-sun:06:00"` |
| `snapshot_retention_days` | Snapshot retention in days | `number` | `7` |
| `snapshot_window` | Daily snapshot window | `string` | `"03:00-04:00"` |
| `tags` | Tags for all resources | `map(string)` | `{}` |

## Outputs

| Name | Description |
|------|-------------|
| `replication_group_arn` | Pass to deploy-bot module as `elasticache_replication_group_arn` |
| `iam_user_arn` | Pass to deploy-bot module as `elasticache_user_arn` |
| `primary_endpoint` | Primary endpoint address |
| `kms_key_arn` | KMS key ARN used for encryption |
