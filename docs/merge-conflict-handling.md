# Merge Conflict Handling

## Scenario

Between the time a deploy PR is created and when it is approved (or
auto-deployed), the default branch may have advanced -- typically because
another deploy PR was merged. GitHub refuses to merge the stale PR, returning
a merge conflict error.

This is most likely in automatic deploy scenarios (high event rate, no human
delay between PR creation and merge) but can also affect long-pending
user-initiated deploys.

## Why Conflicts Are Almost Always Auto-Resolvable

Every deploy PR touches exactly one line in one file: the `newTag:` value in
the app's kustomization.yaml. Each app has its own kustomization path, so
deploys for different apps never touch the same file. The only realistic
conflict sources are:

- A deploy for the **same app** merged while this PR was open (should not
  happen -- the deploy lock prevents concurrent deploys for the same app, but
  could occur if the lock expired on a long-pending deploy).
- A **manual change** to the same kustomization file pushed directly to the
  default branch.

In both cases the resolution is deterministic: re-fetch the current file from
the default branch HEAD, re-apply the `newTag:` substitution, and update the
PR branch. No human judgment is needed.

## Retry Flow

When a merge fails due to a conflict, the bot attempts automatic resolution:

```
attempt 1: merge -> conflict detected
  -> rebase the deploy branch onto current HEAD
    -> tag already current on HEAD: close PR, release lock, notify (no-op path)
    -> rebase error: notify failure (see below)
    -> rebase success: brief pause (GitHub recalculates mergeability), retry merge
attempt 2: merge -> conflict again
  -> notify conflict unresolvable (see below)
attempt 2: merge -> success
  -> deploy completes normally
```

The bot makes at most one rebase attempt before giving up. Two consecutive
conflicts on the same PR indicate something unusual (concurrent deploys, CI
blocking merge, branch protection race) that warrants human intervention.

The brief pause after rebasing is needed because GitHub recalculates
mergeability asynchronously after a branch update. A 2-3 second wait before
retrying the merge is sufficient in practice.

## Failure Notification

### User-Initiated Path (approval required)

If the rebase attempt fails or the retry merge also conflicts:

- The deploy state is reset to pending in Redis (the PR is still open, the
  deploy is still in progress).
- The lock is **not** released (the deploy is still in flight).
- The approver receives a DM:
  > Merge of PR #456 (`myapp` `v1.2.3`) failed due to a conflict that could
  > not be auto-resolved. The PR is still open -- please check it on GitHub
  > and re-approve once the branch is updated.
- A message is posted to the deploy channel (so the requester also sees it):
  > Merge conflict on PR #456 (`myapp` `prod` `v1.2.3`) -- auto-resolution
  > failed. Approver notified.

### Auto-Deploy Path

If the rebase attempt fails or the retry merge also conflicts:

- The PR is closed and the deploy branch is deleted.
- The lock is released.
- A message is posted to the deploy channel (mentioning
  `auto_deploy_approver_group` if set):
  > Auto-deploy of `myapp` `v1.2.3` failed -- merge conflict could not be
  > auto-resolved. Branch has been cleaned up. Manual deploy may be needed.
- An audit log entry is written with `event_type: "conflict_failed"`.
