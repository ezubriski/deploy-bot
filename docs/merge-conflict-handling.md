# Merge Conflict Handling

## Scenario

Between the time a deploy PR is created and when it is approved (or
auto-deployed), the default branch may have advanced — typically because
another deploy PR was merged. GitHub will refuse to merge the stale PR and
return HTTP 405 "Pull Request is not mergeable."

This is most likely in automatic deploy scenarios (high event rate, no human
delay between PR creation and merge) but can also affect long-pending
user-initiated deploys.

## Why Conflicts Are Almost Always Auto-Resolvable

Every deploy PR touches exactly one line in one file: the `newTag:` value in
the app's kustomization.yaml. Each app has its own kustomization path, so
deploys for different apps never touch the same file. The only realistic
conflict sources are:

- A deploy for the **same app** merged while this PR was open (shouldn't
  happen — the deploy lock prevents concurrent deploys for the same app, but
  could occur if the lock expired on a long-pending deploy).
- A **manual change** to the same kustomization file pushed directly to the
  default branch.

In both cases the resolution is deterministic: re-fetch the current file from
the default branch HEAD, re-apply our `newTag:` substitution, and update the
PR branch. No human judgment needed.

## Sentinel Error

Add `ErrMergeConflict` to `internal/github`, detected in `MergePR` when
GitHub returns HTTP 405:

```go
var ErrMergeConflict = errors.New("pull request is not mergeable")

// inside MergePR, before returning the raw error:
var ghErr *gh.ErrorResponse
if errors.As(err, &ghErr) && ghErr.StatusCode == 405 {
    return fmt.Errorf("merge PR: %w", ErrMergeConflict)
}
```

## New GitHub Client Method: `RebaseDeployBranch`

Add to `internal/github/pr.go`:

```go
// RebaseDeployBranch re-applies params.Tag onto the current HEAD of
// params.BaseBranch, updating the deploy branch in place. Call this after
// a MergePR returns ErrMergeConflict to bring the branch up to date, then
// retry MergePR.
func (c *Client) RebaseDeployBranch(ctx context.Context, params CreatePRParams) error
```

Steps:
1. Get current default branch HEAD SHA.
2. Fetch `params.KustomizePath` at that SHA (not the branch — we want the
   current base content, not the potentially stale branch content).
3. Apply `updateNewTag` to produce the updated content.
4. If `updated == content`, the tag is already current on the base branch —
   return `ErrNoChange` (the no-op case; caller should close the PR and clean
   up).
5. Commit the updated file to the deploy branch. Because the branch tip is
   behind HEAD, we can't use a fast-forward commit via `UpdateFile` directly.
   Instead, create the new tree and commit manually via the Git Data API:
   - Create blob from updated content.
   - Create tree based on the current HEAD tree with the blob substituted.
   - Create commit with the current HEAD SHA as parent.
   - Update the branch ref to the new commit SHA (force update).
6. Return nil on success.

### Why the Git Data API instead of UpdateFile

`Repositories.UpdateFile` requires providing the current file's blob SHA on
the branch. After a force-update via ref manipulation, the branch tip will be
a new commit whose parent is current HEAD — but `UpdateFile` doesn't support
creating a commit with an arbitrary parent. The Git Data API
(`CreateBlob` / `CreateTree` / `CreateCommit` / `UpdateRef`) gives the
necessary control.

## Retry Flow

In `bot.handleApprove` and `bot.handleECRPush` (auto-deploy path), after
`MergePR` returns `ErrMergeConflict`:

```
attempt 1: MergePR → ErrMergeConflict
  → RebaseDeployBranch
    → ErrNoChange: close PR, release lock, notify (no-op path)
    → error: notify failure, leave PR open, reset state to pending
    → nil: brief pause (GitHub mergeability recalculation), retry merge
attempt 2: MergePR → ErrMergeConflict
  → notify conflict unresolvable, leave PR open, reset state to pending
attempt 2: MergePR → nil: success
```

Maximum one rebase attempt before giving up. Two consecutive conflicts on the
same PR indicate something unusual (concurrent deploys, CI blocking merge,
branch protection race) that warrants human intervention.

The brief pause after `RebaseDeployBranch` is needed because GitHub
recalculates mergeability asynchronously after a branch update. A 2–3 second
sleep before the retry merge is sufficient in practice.

## Failure Notification

### User-Initiated Path (approval required)

If the rebase attempt fails or the retry merge also conflicts:

- Reset state to `StatePending` in Redis (PR is still open, deploy is still
  in progress).
- Do **not** release the lock (the deploy is still in flight).
- DM the approver:
  > Merge of PR #456 (`myapp` `v1.2.3`) failed due to a conflict that could
  > not be auto-resolved. The PR is still open — please check it on GitHub and
  > re-approve once the branch is updated.
- Post to `deploy_channel` (so the requester also sees it):
  > Merge conflict on PR #456 (`myapp` `prod` `v1.2.3`) — auto-resolution
  > failed. Approver notified.

### Auto-Deploy Path

If the rebase attempt fails or the retry merge also conflicts:

- Close the PR.
- Delete the deploy branch.
- Release the lock.
- Post to `deploy_channel` (mentioning `auto_deploy_approver_group` if set):
  > Auto-deploy of `myapp` `v1.2.3` failed — merge conflict could not be
  > auto-resolved. Branch has been cleaned up. Manual deploy may be needed.
- Audit log entry with `event_type: "conflict_failed"`.

## Test Plan

### Unit: `internal/github/pr_test.go`

`TestMergePR_ConflictReturnsErrMergeConflict` — mock `PullRequests.Merge` to
return a 405 `ErrorResponse`. Assert `MergePR` returns an error wrapping
`ErrMergeConflict`.

`TestRebaseDeployBranch_Success` — mock `GetRef`, `GetContents`, and the Git
Data API calls (`CreateBlob`, `CreateTree`, `CreateCommit`, `UpdateRef`).
Assert the new commit's parent is the current HEAD SHA and the content
contains the updated tag.

`TestRebaseDeployBranch_AlreadyCurrent` — mock `GetContents` to return content
already containing the target tag. Assert `RebaseDeployBranch` returns
`ErrNoChange`.

### Unit: `internal/bot/actions_test.go`

`TestHandleApprove_ConflictAutoResolved` — stub `MergePR` to fail once with
`ErrMergeConflict`, then succeed; stub `RebaseDeployBranch` to return nil.
Assert the deploy completes normally (lock released, store entry deleted,
requester notified of success).

`TestHandleApprove_ConflictUnresolvable` — stub `MergePR` to always return
`ErrMergeConflict`. Assert state reset to `StatePending`, lock not released,
approver DM sent.

### Integration: `tests/integration/deploy_test.go`

`TestDeployMergeConflict` — sequence:
1. Enqueue a deploy request for `env.app` / `env.tag`. Wait for PR.
2. Using the GitHub client directly, create a commit on the default branch
   that touches the same kustomization file (simulate a concurrent merge).
3. Inject approve. Poll until the deploy completes (auto-rebase succeeds and
   merge goes through) or until the conflict-failed notification is detected.
4. Assert exactly one deploy completed; no stale branch left on GitHub.

This test requires the integration environment's GitHub token to have write
access to the default branch, which it already has for the test repo.

## Implementation Order

1. Add `ErrMergeConflict` to `internal/github`; detect in `MergePR`.
2. Implement `RebaseDeployBranch` using the Git Data API.
3. Add conflict retry loop in `bot.handleApprove`.
4. Add conflict retry loop in `bot.handleECRPush` (auto-deploy path, once
   implemented).
5. Unit tests for all three layers.
6. Integration test.
