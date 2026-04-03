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

**ECR-triggered no-op** -- posted to the deploy channel, mentioning the
`auto_deploy_approver_group` if one is configured:

> <!subteam^S01234567> `myapp` (`prod`) is already running `v1.2.3` -- no changes to deploy. No PR created.

Without a group configured:

> `myapp` (`prod`) is already running `v1.2.3` -- no changes to deploy. No PR created.

The group mention ensures the people who would have been asked to approve an
ECR-triggered deploy are still aware the image arrived -- even though no
action is needed.

**User-initiated no-op** -- posted to the deploy channel (without a group
mention) and the requester receives a DM with the same message.

### Slack mention format

`auto_deploy_approver_group` stores a Slack ID directly -- no handle or API
lookup is needed at notification time:

- Channel ID (`C...`) -- the bot posts the no-op notice to that channel
  instead of `deploy_channel`.
- User group ID (`S...`) -- the bot posts to `deploy_channel` with a
  `<!subteam^S...>` mention embedded in the message.

The `C`/`S` prefix disambiguates at runtime. No additional Slack scopes are
required beyond what the bot already holds.

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
