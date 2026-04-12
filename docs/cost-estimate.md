# AWS Cost Estimate

Approximate monthly costs for the AWS resources deploy-bot adds on top
of an existing EKS cluster. All prices are us-east-1, on-demand, as of
mid-2026. Deploy-bot's data volume is tiny — cost is almost entirely
instance hours, not usage.

## Monthly cost by tier

The only line items that differ between tiers are the two database
instances. Everything else is either fixed at a few dollars or rounds
to zero regardless of volume.

| Component | Dev / Staging | Production |
|---|---|---|
| **RDS Postgres** | **$15** | **$50** |
| | db.t4g.micro, single-AZ, 20GB gp3 | db.t4g.small, Multi-AZ, 20GB gp3 |
| **Redis** | **$0** | **$25** |
| | In-cluster pod | cache.t4g.micro × 2, Multi-AZ |
| Secrets Manager | $1 | $1 |
| KMS | $1 | $1 |
| SQS + EventBridge + S3 audit | < $1 | < $1 |
| | | |
| **Total** | **~$17/mo** | **~$77/mo** |

## Why volume doesn't matter

Deploy-bot's usage-priced services stay in free tier or round to zero
at every realistic deploy volume:

| Service | At 200 deploys/day (high) | Free tier / baseline |
|---|---|---|
| SQS | ~100K messages/mo | 1M messages/mo free |
| EventBridge | ~6K events/mo | $1 per 1M events |
| S3 audit | ~6K objects/mo (~6MB) | pennies; Glacier after 90d |
| RDS I/O | single-digit QPS at peak | gp3 baseline: 3000 IOPS |

Even at 500+ deploys/day, none of these cross a dollar. The tables
above show a single cost per tier because the number doesn't change.

## Sizing recommendations

| Component | Dev / Staging | Production | When to upsize |
|---|---|---|---|
| RDS | db.t4g.micro, single-AZ | db.t4g.small, Multi-AZ | Unlikely. 100K deploys/year ≈ 100MB of data, single-digit QPS. Multi-AZ is for availability, not capacity. |
| Redis | In-cluster pod | cache.t4g.micro × 2 | Unlikely. Sub-MB working set (streams + locks + caches). Micro is the smallest ElastiCache offers. Multi-AZ is for pod-failure availability, not durability (Redis is ephemeral). |
| SQS | Standard queue, SSE-SQS | Standard queue, CMK if compliance requires | No sizing dimension — SQS scales automatically. |
| S3 audit | Glacier lifecycle at 90d | Object Lock (WORM), Glacier at 90d, access logging | ~70MB/year at 200 deploys/day. Glacier: $0.004/GB/mo. |

## What's not in this estimate

These are shared infrastructure costs that deploy-bot uses but doesn't
own. Included here so the full picture is visible when budgeting.

| Component | Typical cost | Notes |
|---|---|---|
| EKS control plane | $73/mo | Per-cluster. Assumed shared. |
| NAT Gateway | $32/mo + $0.045/GB | Per-AZ. Often the largest forgotten line item in any VPC-hosted workload. Not deploy-bot-specific. |
| ECR image storage | < $1/mo | Bot image is ~40MB. |
| Data transfer | ~$0 | All traffic is intra-VPC or outbound to AWS APIs. The bot moves KB, not GB. VPC endpoints eliminate the per-GB charge entirely. |

## The honest summary

Deploy-bot's incremental AWS bill is **~$17/mo in dev, ~$77/mo in
prod**. The cost is two small database instances. Everything else
rounds to zero. The number doesn't meaningfully change whether you're
doing 5 deploys/day or 500.
