# ECR Push-Triggered Deploys

## Overview

Allow the bot to receive ECR image push notifications for configured applications and automatically initiate a deploy workflow — either requesting human approval or deploying autonomously depending on per-app and global configuration.

## Event Pipeline

ECR push events flow through the same Redis Stream pipeline as Slack events:

```
EventBridge → SQS → ecrpoller (leader) → buffer → Redis Stream → worker → bot.HandleEvent
```

This gives ECR-triggered deploys the same delivery guarantees as Slack events:
- If Redis is temporarily unavailable, the buffer retries with backoff until the SQS visibility timeout would expire (configurable per queue; default 30s is too short — set to several minutes).
- The worker's consumer group ensures each event is processed exactly once even with multiple worker replicas.
- Crashed workers are recovered via `XAUTOCLAIM`.

The `ecrpoller` is the "receiver" for ECR events, analogous to `cmd/receiver` for Slack events. It runs in the worker process under the leader context (only one pod polls SQS at a time). SQS messages are deleted only after the event has been successfully written to Redis (or buffered).

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
- `repository-name` — matched against the short name portion of `ecr_repo`
- `image-tag` — validated against `tag_pattern`; non-matching tags are discarded before enqueueing
- `registry-id` — AWS account ID (sanity check, optional)

## Queue Changes

### New Envelope Type

Add `EventTypeECRPush = "ecr_push"` to the queue package's event type constants. The envelope `Data` field carries a new `ECRPushEvent` struct:

```go
// internal/queue/ecrevent.go
type ECRPushEvent struct {
    App        string `json:"app"`         // matched app name from config
    Tag        string `json:"tag"`         // pushed image tag
    Repository string `json:"repository"`  // full ECR repository URI
}
```

`decode()` in `queue.go` gains a new case for `EventTypeECRPush` that unmarshals into `ECRPushEvent`.

`bot.HandleEvent` gains a new dispatch branch for `EventTypeECRPush` that calls `bot.handleECRPush(ctx, ECRPushEvent)`.

Tag matching and lock checks happen in the poller **before** enqueueing — discard-before-enqueue keeps the stream clean. The worker handler trusts that by the time it sees an `ecr_push` event, the tag matched and no lock was held at enqueue time. A second lock check in the handler handles the race where another deploy started between enqueue and processing.

## Configuration Changes

### Global (`config.json` top level)

```json
{
  "deployment": {
    "allow_prod_auto_deploy": false
  },
  "ecr_events": {
    "sqs_queue_url": "https://sqs.us-east-1.amazonaws.com/123456789012/deploy-bot-ecr-events",
    "poll_interval": "30s"
  }
}
```

| Field | Default | Description |
|---|---|---|
| `deployment.allow_prod_auto_deploy` | `false` | If false, `auto_deploy: true` is ignored (with a warning) for apps whose `environment` is `"prod"` or `"production"` |
| `ecr_events.sqs_queue_url` | `""` (disabled) | SQS queue URL to poll for ECR push events. Feature is disabled if empty |
| `ecr_events.poll_interval` | `"30s"` | How often to long-poll the SQS queue |

### Per-App (`apps[]`)

```json
{
  "app": "myapp",
  "environment": "prod",
  "auto_deploy": false,
  "auto_deploy_approver_group": "C01234567",
  ...
}
```

| Field | Default | Description |
|---|---|---|
| `auto_deploy` | `false` | When `true`, matching pushes deploy automatically without human approval. Subject to `allow_prod_auto_deploy` global guard |
| `auto_deploy_approver_group` | `""` | Slack ID to @mention when requesting approval for an ECR-triggered deploy. Use a channel ID (`C…`) to post there, or a user group ID (`S…`) to mention the group in `deploy_channel`. Falls back to posting to `deploy_channel` without a mention if unset |

## Behavior

### Tag Matching (in poller, before enqueue)

On each SQS message:
1. Parse the EventBridge envelope; extract `repository-name` and `image-tag`.
2. Find the app config whose `ecr_repo` contains `repository-name` (suffix match).
3. Validate `image-tag` against `tag_pattern`. Discard if no match (build intermediates, `latest`, cache layers, etc.).
4. Check deploy lock (`IsLocked`). If already locked, discard and delete the SQS message — no point queuing a deploy that will immediately be rejected.
5. Enqueue an `ECRPushEvent` to Redis (via buffer). Delete the SQS message only after successful enqueue.

### Worker Handler

`bot.handleECRPush(ctx, evt ECRPushEvent)`:
1. Re-check lock (race: another deploy may have started since enqueue). Bail if locked.
2. Apply prod auto-deploy guard — if the app is prod and `allow_prod_auto_deploy` is false, force `auto_deploy = false`.
3. Create GitHub PR (same as user-initiated).
4. If `auto_deploy` is false: post Slack notification to `auto_deploy_approver_group` (or `deploy_channel`) requesting approval. Approval/rejection from here is identical to the existing interactive button flow.
5. If `auto_deploy` is true: immediately merge the PR, post completion notification to `deploy_channel`, write audit log entry.

### Approval-Required Path (default)

Slack message posted to `auto_deploy_approver_group`:
> New image `myapp:v1.2.3` detected in ECR. Deploy PR #456 is ready for review. [Approve] [Reject]

Requester identity uses a sentinel: Slack user ID `""`, display name `"ECR"`, so audit logs and Slack messages are clearly attributed as bot-initiated.

### Auto-Deploy Path (`auto_deploy: true`)

1. Bot creates a GitHub PR.
2. Bot immediately merges the PR (reuses the existing `approveDeploy` logic with the ECR sentinel as approver identity).
3. Posts to `deploy_channel`: `Auto-deployed myapp:v1.2.3 (ECR push). PR #456 merged.`
4. Audit log: `trigger: "ecr-push"`, `auto_deploy: true`.

### Production Guard

On startup, for each app where `environment` is `"prod"` or `"production"` and `auto_deploy: true`:
- If `allow_prod_auto_deploy` is false: log `WARN` and treat as approval-required at runtime.
- If `allow_prod_auto_deploy` is true: log `INFO` listing all prod apps with auto-deploy active. Also written to audit log.

### No-Op Pushes

An ECR push may fire for a tag that is already the current value in
kustomization.yaml (e.g. image re-pushed, or a manual deploy already applied
it). `CreateDeployPR` detects this before writing to GitHub and returns
`ErrNoChange`. The poller handler treats this identically to a lock-held
discard: release lock, log, post a brief notice to the deploy channel. No PR
is created, no Redis entry is written.

See [no-op-deploy-handling.md](no-op-deploy-handling.md) for the full design,
including the user-initiated path and test plan.

### Duplicate / Rapid Push Protection

The existing per-app deploy lock covers the primary case. The poller discards events when the lock is already held, so rapid successive pushes for the same app result in at most one pending deploy at a time.

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

Currently the audit logger requires an S3 bucket. Change: if `aws.audit_bucket` is empty, the audit logger writes structured log lines via `zap` at `INFO` level instead of sending to S3. Makes audit logging usable in dev/staging without AWS infrastructure.

Implementation: extract a `Logger` interface from the existing S3 implementation; provide a `zapLogger` that writes to the existing zap instance. `audit.NewLogger` returns the zap implementation when `audit_bucket` is empty.

## New / Modified Packages

| Path | Change | Purpose |
|---|---|---|
| `internal/ecrpoller/poller.go` | New | SQS long-poll loop; parses EventBridge ECR events, matches app config, validates tag, checks lock, enqueues to Redis via buffer |
| `internal/ecrpoller/event.go` | New | EventBridge `ECR Image Action` event struct |
| `internal/queue/ecrevent.go` | New | `ECRPushEvent` struct and `EventTypeECRPush` constant |
| `internal/queue/queue.go` | Modify | Add `EventTypeECRPush` case to `decode()` |
| `internal/bot/ecr.go` | New | `handleECRPush` method, prod-guard logic, auto-deploy merge path, Slack notifications |
| `internal/bot/handler.go` | Modify | Dispatch `EventTypeECRPush` to `handleECRPush` |
| `internal/audit/logger.go` | Modify | Extract interface; add `zapLogger` fallback |
| `cmd/bot/main.go` | Modify | Start ecrpoller under leader context; startup prod-guard logging |

The `ecrpoller` uses the existing `buffer.Buffer` for Redis backpressure, taking the same `*redis.Client` the rest of the process uses.

## IAM Additions

The bot role needs:

```json
{
  "Sid": "ReceiveECREvents",
  "Effect": "Allow",
  "Action": [
    "sqs:ReceiveMessage",
    "sqs:DeleteMessage",
    "sqs:GetQueueAttributes"
  ],
  "Resource": "arn:aws:sqs:<region>:<account>:deploy-bot-ecr-events"
}
```

EventBridge sends to SQS via a queue resource policy (not the bot IAM policy). The SQS message visibility timeout should be set to at least the buffer's maximum retry window (e.g. 5 minutes) so that a Redis outage does not cause SQS to redeliver the message to a second leader before the first has buffered it.

## Implementation Order

1. **Audit log fallback** — extract `Logger` interface; add `zapLogger`; update `audit.NewLogger`. No config changes, no caller changes.
2. **Queue envelope** — add `ECRPushEvent` struct and `EventTypeECRPush` constant; add decode case in `queue.go`; add dispatch case in `bot.HandleEvent`.
3. **Config additions** — `AutoDeploy`, `AutoDeployApproverGroup` on `AppConfig`; `AllowProdAutoDeploy` on `DeploymentConfig`; `ECREvents` struct on top-level config.
4. **Startup prod-guard logging** — iterate apps in `cmd/bot/main.go` after config load; log + audit.
5. **`ecrpoller` package** — SQS poll loop, EventBridge envelope parsing, app/tag matching, lock check, enqueue via buffer.
6. **`bot.handleECRPush`** — second lock check, prod guard, PR creation, auto-deploy or approval-request branch.
7. **Slack notification templates** — approval-request message with Approve/Reject buttons; auto-deploy completion message.
8. **Wire into `cmd/bot/main.go`** — construct buffer for poller, start poller goroutine under leader context.
9. **Unit tests** — poller tag matching/filtering, `handleECRPush` with mocked store/github/slack; audit fallback.
10. **Integration test** — call poller's enqueue path directly (no real SQS); verify PR creation and auto-deploy/approval flow end-to-end.
