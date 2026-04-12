# AWS Cost Estimate

Approximate costs for the AWS resources deploy-bot adds on top of an
existing EKS cluster. All prices are us-east-1, on-demand, as of
mid-2026. Your actual bill will vary with region, reserved-instance
or savings-plan discounts, negotiated pricing, and how your
organization allocates shared costs like EKS and NAT Gateways. The
figures below are based on published list prices and are intended to
give a reasonable ballpark, not a quote — verify against the
[AWS pricing calculator](https://calculator.aws/) for your specific
configuration.

## The short version

Deploy-bot's cost is two small database instances. Everything else
rounds to zero regardless of deploy volume. The question is how much
availability redundancy you want around those instances.

| | Standard | High availability |
|---|---|---|
| **Monthly** | **~$17** | **~$77** |
| **Annual** | **~$204** | **~$924** |

Both configurations run the same code, the same schema, and the same
features. The difference is what happens during infrastructure
failures — read on for what that actually means in practice.

## Cost breakdown

| Component | Standard | High availability |
|---|---|---|
| **RDS Postgres** | **$15/mo** | **$50/mo** |
| | db.t4g.micro, single-AZ | db.t4g.small, Multi-AZ |
| **Redis** | **$0/mo** | **$25/mo** |
| | In-cluster pod | ElastiCache cache.t4g.micro × 2, Multi-AZ |
| Secrets Manager | $1/mo | $1/mo |
| KMS | $1/mo | $1/mo |
| SQS + EventBridge + S3 audit | < $1/mo | < $1/mo |

## What you're paying for (and what you're not)

The cost difference between the two columns is entirely about
**availability during failures** — how quickly the bot resumes after
an infrastructure event. It is not about data safety, capacity, or
features. Both columns:

- Store deploy history and pending deploys in Postgres with automated
  daily backups (included with RDS at no extra cost)
- Run the same bot binary with the same schema
- Handle the same deploy volume (the bot's resource usage is trivial
  at any realistic scale — see [volume](#why-volume-doesnt-matter))

### RDS: single-AZ vs Multi-AZ

| | Standard (single-AZ) | HA (Multi-AZ) |
|---|---|---|
| **Normal operation** | Identical | Identical |
| **Node failure** | AWS restores automatically from latest backup. Deploys pause for minutes while the instance recovers. | Automatic failover to standby replica. Deploys pause for ~30 seconds. |
| **Maintenance window** | Brief downtime during patched restarts (typically seconds, scheduled). | Patched via failover — zero-downtime maintenance. |
| **Data loss risk** | Automated daily backups + point-in-time recovery (5-minute RPO). A failure between backups could lose the most recent few minutes of deploy history. In practice this means a small number of history entries — the deploys themselves (GitHub PRs) are unaffected. | Same backup schedule, but the synchronous standby replica means zero data loss on failover. |

The realistic impact of a single-AZ RDS failure: deploys pause for a
few minutes, then resume. No PRs are lost. A handful of recent history
entries might be missing until the next deploy recreates them. The bot
reconciles against GitHub on startup, so orphaned PRs are cleaned up
automatically.

### Redis: in-cluster pod vs ElastiCache

Since 3.0, Redis holds only ephemeral data — event streams, locks,
caches, and dedupe markers. **No durable state lives in Redis.** This
means the recovery story for an in-cluster Redis pod is nearly
identical to ElastiCache:

| | Standard (in-cluster pod) | HA (ElastiCache Multi-AZ) |
|---|---|---|
| **Normal operation** | Identical | Identical |
| **Pod/node restart** | Redis restarts in seconds. Streams are empty; Slack retries in-flight events. Locks auto-expire. Caches rebuild on their own. | ElastiCache failover in ~1-2 minutes. Same recovery path (streams re-deliver, locks expire, caches rebuild). |
| **Data loss** | Streams only — no deploy history or pending state is affected (those are in Postgres). | Same — ElastiCache persistence is disabled by default because there's nothing worth persisting. |

The only operational advantage of ElastiCache over an in-cluster pod
is that ElastiCache survives Kubernetes node failures without waiting
for pod rescheduling. In practice the difference is seconds vs minutes
of deploy unavailability.

An in-cluster Redis pod is a legitimate production choice for
deploy-bot. It was not before 3.0, when Redis held durable state.

## Why volume doesn't matter

Deploy-bot's usage-priced services stay in free tier or round to zero
at every realistic deploy volume:

| Service | At 200 deploys/day | Free tier / baseline |
|---|---|---|
| SQS | ~100K messages/mo | 1M messages/mo free |
| EventBridge | ~6K events/mo | $1 per 1M events |
| S3 audit | ~6K objects/mo (~6MB) | pennies; Glacier after 90d |
| RDS I/O | single-digit QPS at peak | gp3 baseline: 3000 IOPS |

Even at 500+ deploys/day, none of these services cross $1/mo. The
cost tables above show a single number per tier because deploy volume
doesn't change it.

## Sizing

| Component | Standard | High availability | When to upsize |
|---|---|---|---|
| RDS | db.t4g.micro, single-AZ, 20GB gp3 | db.t4g.small, Multi-AZ, 20GB gp3 | Unlikely. 100K deploys/year produces ~100MB of data at single-digit QPS. |
| Redis | In-cluster pod | cache.t4g.micro × 2 | Unlikely. Sub-MB working set. Micro is the smallest ElastiCache offers. |
| SQS | Standard queue, SSE-SQS | Standard queue, CMK if compliance requires | No sizing dimension — SQS scales automatically. |
| S3 audit | Glacier lifecycle at 90d | Object Lock (WORM), Glacier at 90d | ~70MB/year at 200 deploys/day. |

## What's not in this estimate

Shared infrastructure costs that deploy-bot uses but doesn't own.
Included for budgeting visibility.

| Component | Typical cost | Notes |
|---|---|---|
| EKS control plane | $73/mo ($876/yr) | Per-cluster. Assumed shared. |
| NAT Gateway | $32/mo + $0.045/GB | Per-AZ. Often the largest forgotten line item. |
| ECR image storage | < $1/mo | Bot image is ~40MB. |
| Data transfer | ~$0 | Intra-VPC. VPC endpoints eliminate per-GB charges. |

## Summary

| | Standard | High availability |
|---|---|---|
| **Annual cost** | **~$204/yr** | **~$924/yr** |
| **RDS failover** | Minutes (auto-restore) | ~30 seconds |
| **Redis failover** | Seconds (pod restart) | 1-2 minutes (ElastiCache) |
| **Data loss on failure** | Minutes of recent history (RPO ~5m) | None (synchronous replica) |
| **Deploy impact** | Deploys pause briefly, resume on recovery | Near-zero interruption |

Both options are production-viable. The standard configuration is not
a dev setup with known gaps — it's a complete deployment with a small,
well-understood availability trade-off. The choice comes down to
whether a few minutes of deploy downtime during a rare infrastructure
event is acceptable for your organization.
