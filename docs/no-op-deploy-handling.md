# No-Op Deploy Handling

## Scenario

A deploy is requested (by a user or by an ECR push event) for a tag that is
already the current value in the app's kustomization.yaml. The bot would
otherwise create a branch, commit identical file content, and open a PR with
zero file changes — a no-op that wastes a PR and confuses approvers.

This scenario is more likely with ECR-triggered deploys (a push event may fire
for a tag that was manually deployed, or the same image was re-pushed to ECR)
but can also occur with user-initiated deploys.

## Detection Point

`CreateDeployPR` in `internal/github/pr.go` already reads the current file
content and calls `updateNewTag`. The no-op is detectable here, before any
write to GitHub:

```go
updated, err := updateNewTag(content, params.Tag)
if err != nil {
    return 0, "", fmt.Errorf("update kustomization tag: %w", err)
}
if updated == content {
    // Branch was already created; clean it up before returning.
    _ = c.DeleteBranch(ctx, branch)
    return 0, "", ErrNoChange
}
```

`ErrNoChange` is a sentinel exported from `internal/github`:

```go
var ErrNoChange = errors.New("tag is already current; no changes to deploy")
```

The branch is deleted before returning so no stale branches accumulate.

## Handler Behavior

In `bot.handleDeploySubmit` and `bot.handleECRPush` (once implemented), when
`errors.Is(err, githubPkg.ErrNoChange)`:

1. Release the deploy lock — no deploy is in flight.
2. Do NOT store anything in Redis (nothing to track).
3. Post to the configured deploy channel (`slack.deploy_channel`), @mentioning
   the app's `auto_deploy_approver_group` if one is configured:
   > <!subteam^S01234567> `myapp` (`prod`) is already running `v1.2.3` — no changes to deploy. No PR created.
   >
   > Without a group configured: "`myapp` (`prod`) is already running `v1.2.3` — no changes to deploy. No PR created."
4. DM the requester (user-initiated path only) with the same message, without
   the group mention (the requester doesn't need to be paged again).
5. Log at `INFO` level; write an audit log entry with `event_type: "noop"`.

The group mention ensures the people who would have been asked to approve an
ECR-triggered deploy are still aware the image arrived — even though no action
is needed. For user-initiated no-ops the channel post without a group mention
is sufficient; the requester learns via DM.

No history entry is pushed (there is nothing to complete or cancel). The lock
is released immediately so a follow-up deploy of a different tag is not blocked.

### Slack mention format

`auto_deploy_approver_group` stores a Slack ID directly — no handle, no API
lookup needed at notification time:

- Channel ID (`C…`) — bot posts the no-op notice to that channel instead of
  `deploy_channel`.
- User group ID (`S…`) — bot posts to `deploy_channel` with a
  `<!subteam^S…>` mention embedded in the message.

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

## Test Plan

### Unit: `internal/github/pr_test.go`

`TestCreateDeployPR_NoChange` — mock `GetContents` to return kustomization
content that already contains the target tag. Assert `CreateDeployPR` returns
`ErrNoChange` and that `DeleteBranch` was called (branch cleanup).

### Unit: `internal/bot/actions_test.go`

`TestHandleDeploySubmit_NoChange` — stub `CreateDeployPR` to return
`ErrNoChange`. Assert:
- Lock is released.
- `PostMessageContext` called once to the deploy channel with the no-op message.
- DM sent to requester.
- Nothing written to the store.

`TestHandleECRPush_NoChange_WithGroup` — same as above but triggered via the
ECR path with `auto_deploy_approver_group` set. Assert the channel message
contains `<!subteam^GROUPID>` and no DM is sent (no human requester).

### Integration: `tests/integration/deploy_test.go`

`TestDeployNoOp` — inject a deploy request for a tag that is already set in
the kustomization file. Assert:
- No PR is created in GitHub.
- No pending deploy appears in Redis.
- A message appears in the deploy channel within the poll window.

The test must know the currently-deployed tag. Two approaches:
1. Read the kustomization file via the GitHub client before injecting the event.
2. Have the integration env expose a `currentTag` derived from the live file.

Approach 1 is simpler and keeps the test self-contained.

## Implementation Order

This is a small, self-contained change with no external dependencies. It can
be implemented independently of the ECR push feature.

1. Add `ErrNoChange` sentinel to `internal/github`.
2. Add the identity-content check in `CreateDeployPR`; call `DeleteBranch`
   before returning.
3. Handle `ErrNoChange` in `bot.handleDeploySubmit`; notify channel + DM.
4. Add `"noop"` audit event type.
5. Unit tests for both layers.
6. Integration test.
