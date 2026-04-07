# ECR Push-Triggered Deploys

## Overview

The bot receives ECR image push notifications for configured applications and
automatically initiates a deploy workflow -- either requesting human approval
or deploying autonomously depending on per-app and global configuration.

## Event Pipeline

ECR push events flow through a dedicated Redis Stream, separate from user events:

```
EventBridge -> SQS -> ecrpoller (receiver) -> buffer -> ecr:events stream -> worker -> bot handler
```

User events (slash commands, button clicks, mentions) use the `user:events` stream. The worker drains `user:events` with priority before checking `ecr:events`, so user interactions are never delayed by ECR bulk processing.

This gives ECR-triggered deploys the same delivery guarantees as user events:
- If Redis is temporarily unavailable, the buffer retries with backoff until
  the SQS visibility timeout expires.
- The worker's consumer group ensures each event is processed exactly once
  even with multiple worker replicas.
- Crashed workers are recovered via `XAUTOCLAIM`.

SQS messages are deleted only after the event has been successfully written to
Redis (or buffered). The SQS message visibility timeout should be set to at
least the buffer's maximum retry window (e.g. 5 minutes) so that a Redis
outage does not cause SQS to redeliver the message before the first has been
buffered.

### EventBridge Rule

```json
{
  "source": ["aws.ecr"],
  "detail-type": ["ECR Image Action"],
  "detail": {
    "action-type": ["PUSH"],
    "result": ["SUCCESS"]
  }
}
```

Relevant fields in the event `detail`:
- `repository-name` -- matched against the short name portion of `ecr_repo`
- `image-tag` -- validated against `tag_pattern`; non-matching tags are
  discarded before enqueueing
- `registry-id` -- AWS account ID (sanity check, optional)

## Configuration

### Global (`config.json` top level)

```json
{
  "deployment": {
    "allow_prod_auto_deploy": false
  },
  "ecr_auto_deploy": {
    "enabled": true,
    "sqs_queue_url": "https://sqs.us-east-1.amazonaws.com/123456789012/deploy-bot-ecr-events",
    "poll_interval": "30s"
  }
}
```

| Field | Default | Description |
|---|---|---|
| `deployment.allow_prod_auto_deploy` | `false` | If false, `auto_deploy: true` is ignored (with a warning) for apps whose `environment` is `"prod"` or `"production"` |
| `ecr_auto_deploy.enabled` | `false` | Enable ECR push-triggered deploys |
| `ecr_auto_deploy.sqs_queue_url` | `""` | SQS queue URL to poll for ECR push events |
| `ecr_auto_deploy.poll_interval` | `"30s"` | How often to long-poll the SQS queue |

### Per-App (`apps[]`)

```json
{
  "app": "myapp",
  "environment": "prod",
  "auto_deploy": false
}
```

| Field | Default | Description |
|---|---|---|
| `auto_deploy` | `false` | When `true`, matching pushes deploy automatically without human approval. Subject to `allow_prod_auto_deploy` global guard |

All ECR-triggered deploy notifications are posted to `deploy_channel`.

## Behavior

### Tag Matching

On each SQS message, the poller:
1. Parses the EventBridge envelope and extracts `repository-name` and
   `image-tag`.
2. Finds all app configs whose `ecr_repo` contains `repository-name` (suffix
   match). A single ECR push triggers deploys for every app sharing that
   ECR repo (e.g. the same image deployed to dev and prod).
3. Validates `image-tag` against `tag_pattern`. Discards if no match (build
   intermediates, `latest`, cache layers, etc.).
4. Checks the deploy lock. If already locked, discards and deletes the SQS
   message -- there is no point queuing a deploy that will immediately be
   rejected.
5. Enqueues the event to Redis (via the buffer). Deletes the SQS message only
   after successful enqueue.

### Worker Handler

When the worker processes an ECR push event:
1. Re-checks the lock (another deploy may have started between enqueue and
   processing). Bails if locked.
2. Applies the prod auto-deploy guard -- if the app is prod and
   `allow_prod_auto_deploy` is false, treats the deploy as approval-required
   regardless of the app's `auto_deploy` setting.
3. Creates a GitHub PR (same as user-initiated deploys).
4. If `auto_deploy` is false: posts a Slack notification requesting approval.
5. If `auto_deploy` is true: immediately merges the PR and posts a completion
   notification.

### Approval-Required Path (default)

Slack message posted to `deploy_channel`:

> New image `myapp:v1.2.3` detected in ECR. Deploy PR #456 is ready for review. [Approve] [Reject]

Requester identity uses a sentinel (display name `"ECR"`), so audit logs and
Slack messages are clearly attributed as bot-initiated. Approval and rejection
from here follow the same interactive button flow as user-initiated deploys.

### Auto-Deploy Path (`auto_deploy: true`)

1. The bot creates a GitHub PR.
2. The bot immediately merges the PR.
3. Posts to `deploy_channel`: `Auto-deployed myapp:v1.2.3 (ECR push). PR #456 merged.`
4. Audit log entry includes `trigger: "ecr-push"`, `auto_deploy: true`.

### Production Guard

On startup, for each app where `environment` is `"prod"` or `"production"`
and `auto_deploy: true`:
- If `allow_prod_auto_deploy` is false: logs `WARN` and treats the app as
  approval-required at runtime.
- If `allow_prod_auto_deploy` is true: logs `INFO` listing all prod apps with
  auto-deploy active. Also written to the audit log.

### No-Op Pushes

An ECR push may fire for a tag that is already the current value in
kustomization.yaml (e.g. image re-pushed, or a manual deploy already applied
it). The bot detects this before writing to GitHub. The deploy lock is
released, a brief notice is posted to the deploy channel, and no PR is
created.

See [no-op-deploy-handling.md](no-op-deploy-handling.md) for full details.

### Threaded Notifications

When multiple ECR pushes (or a mix of ECR and user deploys) target the same
environment and the number of pending deploys meets the `slack.thread_threshold`
(default 4), the bot posts a parent message ("Processing multiple deployment
requests for *env*...") and threads individual approval requests under it. This
keeps the deploy channel readable during high-volume push bursts. Thread parent
timestamps are stored in Redis as `thread:<env>` keys with a 10-minute TTL,
created atomically (SET NX) to prevent duplicate parent messages from concurrent
workers.

### Duplicate / Rapid Push Protection

The existing per-app deploy lock covers the primary case. The poller discards
events when the lock is already held, so rapid successive pushes for the same
app result in at most one pending deploy at a time.

## Audit Logging

ECR-triggered deploys are audit-logged with additional fields:

```json
{
  "trigger": "ecr-push",
  "auto_deploy": true,
  "image_tag": "v1.2.3",
  "ecr_repository": "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp"
}
```

Startup listing of prod auto-deploy apps:

```json
{
  "event": "startup",
  "prod_auto_deploy_apps": ["myapp-prod", "otherapp-prod"]
}
```

### Audit Log Fallback

If `aws.audit_bucket` is empty, the audit logger writes structured log lines
via zap at `INFO` level instead of sending to S3. This makes audit logging
usable in dev/staging without AWS infrastructure.
