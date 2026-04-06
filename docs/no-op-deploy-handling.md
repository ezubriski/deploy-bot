# No-Op Deploy Handling

## Scenario

A deploy is requested (by a user or by an ECR push event) for a tag that is
already the current value in the app's kustomization.yaml. Without no-op
detection, the bot would create a branch, commit identical file content, and
open a PR with zero file changes -- a no-op that wastes a PR and confuses
approvers.

This scenario is more likely with ECR-triggered deploys (a push event may fire
for a tag that was manually deployed, or the same image was re-pushed to ECR)
but can also occur with user-initiated deploys.

## What Happens

When the bot detects that the requested tag is already the current value in
the kustomization file, it short-circuits the deploy:

1. **No PR is created.** The bot detects the no-op before writing anything to
   GitHub. Any temporary branch is cleaned up automatically.
2. **The deploy lock is released immediately** so a follow-up deploy of a
   different tag is not blocked.
3. **Nothing is stored in Redis.** There is no pending deploy to track.
4. **No history entry is created.** There is nothing to complete or cancel.
5. **A notice is posted to Slack** (see below).
6. **An audit log entry is written** with `event_type: "noop"`.

### Slack notifications

**ECR-triggered no-op** -- posted to the deploy channel:

> `myapp` (`prod`) is already running `v1.2.3` -- no changes to deploy. No PR created.

**User-initiated no-op** -- posted to the deploy channel and the requester
receives a DM with the same message.

## Audit Log Entry

```json
{
  "event_type": "noop",
  "app": "myapp",
  "environment": "prod",
  "tag": "v1.2.3",
  "trigger": "user",
  "requester": "ghlogin"
}
```

For ECR-triggered no-ops, `"trigger": "ecr-push"`.
