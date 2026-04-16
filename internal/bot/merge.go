package bot

import (
	"context"
	"errors"
	"time"

	githubPkg "github.com/ezubriski/deploy-bot/internal/github"
	"go.uber.org/zap"
)

// mergeResult holds the outcome of a mergeWithRetry attempt.
type mergeResult struct {
	SHA string // merge commit SHA on success
	// NoOp is true when a rebase reveals the tag is already on the default
	// branch — the caller should close the PR as a no-op.
	NoOp bool
}

// mergeWithRetry attempts to merge a PR, automatically rebasing on merge
// conflict and retrying once. It handles the conflict→rebase→retry flow
// shared by handleApprove and handleECRAutoDeploy.
//
// On success it returns the merge SHA. If the rebase reveals the deploy is
// a no-op (tag already on default branch), it returns mergeResult{NoOp: true}.
// All other errors are returned to the caller for case-specific handling.
func (b *Bot) mergeWithRetry(ctx context.Context, prNumber int, params githubPkg.CreatePRParams, mergeMethod string) (mergeResult, error) {
	sha, err := b.gh.MergePR(ctx, prNumber, mergeMethod)
	if err == nil {
		return mergeResult{SHA: sha}, nil
	}

	if !errors.Is(err, githubPkg.ErrMergeConflict) {
		return mergeResult{}, err
	}

	// Merge conflict — attempt rebase and retry.
	b.log.Info("merge conflict, attempting rebase", zap.Int("pr", prNumber))
	baseBranch, branchErr := b.gh.GetDefaultBranch(ctx)
	if branchErr != nil {
		b.log.Error("get default branch for rebase", zap.Error(branchErr))
		return mergeResult{}, err // return original merge conflict error
	}

	params.BaseBranch = baseBranch
	rebaseErr := b.gh.RebaseDeployBranch(ctx, params)
	if rebaseErr != nil {
		if errors.Is(rebaseErr, githubPkg.ErrNoChange) {
			return mergeResult{NoOp: true}, nil
		}
		b.log.Error("rebase deploy branch", zap.Int("pr", prNumber), zap.Error(rebaseErr))
		return mergeResult{}, err // return original merge conflict error
	}

	// Give GitHub a moment to recalculate mergeability after the force-push.
	select {
	case <-ctx.Done():
		return mergeResult{}, ctx.Err()
	case <-time.After(3 * time.Second):
	}

	retrySHA, retryErr := b.gh.MergePR(ctx, prNumber, mergeMethod)
	if retryErr != nil {
		b.log.Error("merge PR after rebase", zap.Int("pr", prNumber), zap.Error(retryErr))
		return mergeResult{}, retryErr
	}
	return mergeResult{SHA: retrySHA}, nil
}
