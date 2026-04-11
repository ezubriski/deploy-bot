package bot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/config"
	githubPkg "github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/store"
)

// ecrRequesterID is the sentinel Slack user ID for ECR-triggered deploys.
const ecrRequesterID = ""

// ecrRequesterName is the display name used in audit logs and Slack messages.
const ecrRequesterName = "ECR"

// handleECRPush processes an ECR push event: checks locks, creates a PR, and
// either auto-merges or posts an approval request depending on config.
func (b *Bot) handleECRPush(ctx context.Context, evt queue.ECRPushEvent) {
	cfg := b.cfg.Load()
	appCfg, ok := cfg.AppByName(evt.App)
	if !ok {
		b.log.Error("ecr push: app not found in config", zap.String("app", evt.App))
		return
	}

	env := appCfg.Environment
	deployChannel := cfg.Slack.DeployChannel

	// Re-check lock (race: another deploy may have started since enqueue).
	locked, err := b.store.IsLocked(ctx, env, evt.App)
	if err != nil {
		b.log.Error("ecr push: check lock", zap.String("app", evt.App), zap.Error(err))
		return
	}
	if locked {
		b.log.Info("ecr push: app locked, discarding", zap.String("app", evt.App), zap.String("tag", evt.Tag))
		return
	}

	// Acquire lock.
	lockTTL := cfg.LockTTL()
	acquired, err := b.store.AcquireLock(ctx, env, evt.App, ecrRequesterName, lockTTL)
	if err != nil {
		b.log.Error("ecr push: acquire lock", zap.String("app", evt.App), zap.Error(err))
		return
	}
	if !acquired {
		b.log.Info("ecr push: lock contention, discarding", zap.String("app", evt.App), zap.String("tag", evt.Tag))
		return
	}

	// Create PR.
	baseBranch, err := b.gh.GetDefaultBranch(ctx)
	if err != nil {
		b.log.Error("ecr push: get default branch", zap.Error(err))
		b.errIfErr("store: release lock", b.store.ReleaseLock(ctx, env, evt.App), zap.String("env", env), zap.String("app", evt.App))
		return
	}

	prNumber, prURL, err := b.gh.CreateDeployPR(ctx, githubPkg.CreatePRParams{
		App:           evt.App,
		Environment:   env,
		Tag:           evt.Tag,
		KustomizePath: appCfg.KustomizePath,
		BaseBranch:    baseBranch,
		Requester:     ecrRequesterName,
		Reason:        fmt.Sprintf("ECR push: %s:%s", evt.Repository, evt.Tag),
	})
	if err != nil {
		b.errIfErr("store: release lock", b.store.ReleaseLock(ctx, env, evt.App), zap.String("env", env), zap.String("app", evt.App))
		if errors.Is(err, githubPkg.ErrNoChange) {
			noopMsg := fmt.Sprintf("`%s` (`%s`) is already running `%s` — no changes to deploy (ECR push). No PR created.", evt.App, env, evt.Tag)
			b.postNoOpNotice(ctx, evt.App, noopMsg)
			if err := b.auditLog.Log(ctx, audit.AuditEvent{
				EventType:   audit.EventNoop,
				Trigger:     audit.TriggerECRPush,
				App:         evt.App,
				Environment: env,
				Tag:         evt.Tag,
			}); err != nil {
				b.log.Error("audit log", zap.Error(err))
			}
			b.log.Info("ecr push no-op: tag already current", zap.String("app", evt.App), zap.String("tag", evt.Tag))
			return
		}
		b.log.Error("ecr push: create deploy PR", zap.String("app", evt.App), zap.Error(err))
		b.postSlack(ctx, deployChannel, "notice",
			slack.MsgOptionText(fmt.Sprintf(
				"ECR push for *%s* (%s) `%s` failed: could not create PR: %v",
				evt.App, env, evt.Tag, err,
			), false),
		)
		return
	}

	autoDeploy := appCfg.EffectiveAutoDeploy(cfg.Deployment.AllowProdAutoDeploy)
	if autoDeploy {
		b.handleECRAutoDeploy(ctx, evt, appCfg, cfg, prNumber, prURL)
	} else {
		b.handleECRApprovalRequest(ctx, evt, appCfg, cfg, prNumber, prURL)
	}
}

// handleECRAutoDeploy immediately merges the PR and posts a completion notice.
// On merge conflict, it attempts a rebase and retries once before notifying.
func (b *Bot) handleECRAutoDeploy(ctx context.Context, evt queue.ECRPushEvent, appCfg *config.AppConfig, cfg *config.Config, prNumber int, prURL string) {
	env := appCfg.Environment
	deployChannel := cfg.Slack.DeployChannel

	mergeSHA, mergeErr := b.gh.MergePR(ctx, prNumber, cfg.Deployment.MergeMethod)
	if mergeErr != nil {
		if !errors.Is(mergeErr, githubPkg.ErrMergeConflict) {
			b.log.Error("ecr auto-deploy: merge failed", zap.Int("pr", prNumber), zap.Error(mergeErr))
			b.notifyECRAutoDeployFailed(ctx, evt, appCfg, cfg, prNumber, prURL, mergeErr)
			return
		}

		// Merge conflict — attempt rebase and retry.
		b.log.Info("ecr auto-deploy: merge conflict, attempting rebase", zap.Int("pr", prNumber))
		baseBranch, err := b.gh.GetDefaultBranch(ctx)
		if err != nil {
			b.log.Error("ecr auto-deploy: get default branch for rebase", zap.Error(err))
			b.notifyECRAutoDeployFailed(ctx, evt, appCfg, cfg, prNumber, prURL, mergeErr)
			return
		}

		rebaseErr := b.gh.RebaseDeployBranch(ctx, githubPkg.CreatePRParams{
			App:           evt.App,
			Environment:   env,
			Tag:           evt.Tag,
			KustomizePath: appCfg.KustomizePath,
			BaseBranch:    baseBranch,
		})
		if rebaseErr != nil {
			if errors.Is(rebaseErr, githubPkg.ErrNoChange) {
				// Tag already on default branch — close as no-op.
				b.warnIfErr("github: comment no-op", b.gh.CommentNoOp(ctx, prNumber, evt.App, evt.Tag), zap.Int("pr", prNumber))
				b.warnIfErr("github: close PR", b.gh.ClosePR(ctx, prNumber), zap.Int("pr", prNumber))
				b.errIfErr("store: release lock", b.store.ReleaseLock(ctx, env, evt.App), zap.String("env", env), zap.String("app", evt.App))
				b.log.Info("ecr auto-deploy: no-op after rebase, tag already current", zap.String("app", evt.App))
				return
			}
			b.log.Error("ecr auto-deploy: rebase failed", zap.Int("pr", prNumber), zap.Error(rebaseErr))
			b.notifyECRAutoDeployFailed(ctx, evt, appCfg, cfg, prNumber, prURL, rebaseErr)
			return
		}

		// Give GitHub a moment to recalculate mergeability after the force-push.
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}

		retrySHA, retryErr := b.gh.MergePR(ctx, prNumber, cfg.Deployment.MergeMethod)
		if retryErr != nil {
			b.log.Error("ecr auto-deploy: merge failed after rebase", zap.Int("pr", prNumber), zap.Error(retryErr))
			b.notifyECRAutoDeployFailed(ctx, evt, appCfg, cfg, prNumber, prURL, retryErr)
			return
		}
		mergeSHA = retrySHA
	}

	var wg sync.WaitGroup
	wg.Add(6)
	go func() {
		defer wg.Done()
		b.warnIfErr("github: comment approved", b.gh.CommentApproved(ctx, prNumber, ecrRequesterName), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.warnIfErr("github: remove pending label", b.gh.RemoveLabel(ctx, prNumber, cfg.PendingLabel()), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.errIfErr("store: release lock", b.store.ReleaseLock(ctx, env, evt.App), zap.String("env", env), zap.String("app", evt.App))
	}()
	go func() {
		defer wg.Done()
		autoDeployOpts := []slack.MsgOption{
			slack.MsgOptionText(fmt.Sprintf(
				"Auto-deployed *%s* (%s) `%s` (ECR push). <%s|PR #%d> merged.",
				evt.App, env, evt.Tag, prURL, prNumber,
			), false),
		}
		autoDeployOpts = append(autoDeployOpts, threadOption(b.getThreadTS(ctx, env))...)
		b.postSlack(ctx, deployChannel, "notice", autoDeployOpts...)
	}()
	go func() {
		defer wg.Done()
		if err := b.auditLog.Log(ctx, audit.AuditEvent{
			EventType:   audit.EventApproved,
			Trigger:     audit.TriggerECRPush,
			App:         evt.App,
			Environment: env,
			Tag:         evt.Tag,
			PRNumber:    prNumber,
			PRURL:       prURL,
			AutoDeploy:  true,
		}); err != nil {
			b.log.Error("audit log", zap.Error(err))
		}
	}()
	go func() {
		defer wg.Done()
		if err := b.store.PushHistory(ctx, store.HistoryEntry{
			EventType:       audit.EventApproved,
			App:             evt.App,
			Environment:     env,
			Tag:             evt.Tag,
			PRNumber:        prNumber,
			PRURL:           prURL,
			RequesterID:     ecrRequesterID,
			CompletedAt:     time.Now(),
			GitopsCommitSHA: mergeSHA,
		}); err != nil {
			b.log.Warn("store: push history", zap.Error(err))
		}
	}()
	b.metrics.RecordDeploy(evt.App, audit.EventApproved)
	wg.Wait()
	b.updatePendingGauge(ctx)
	b.log.Info("ecr auto-deploy complete", zap.String("app", evt.App), zap.String("tag", evt.Tag), zap.Int("pr", prNumber))
}

// handleECRApprovalRequest stores the pending deploy and posts an approval
// request with Approve/Reject buttons.
func (b *Bot) handleECRApprovalRequest(ctx context.Context, evt queue.ECRPushEvent, appCfg *config.AppConfig, cfg *config.Config, prNumber int, prURL string) {
	env := appCfg.Environment
	reason := fmt.Sprintf("ECR push: %s:%s", evt.Repository, evt.Tag)

	staleDuration := cfg.StaleDuration()
	expiresAt := time.Now().Add(staleDuration)

	d := &store.PendingDeploy{
		App:         evt.App,
		Environment: env,
		Tag:         evt.Tag,
		PRNumber:    prNumber,
		PRURL:       prURL,
		Requester:   ecrRequesterName,
		RequesterID: ecrRequesterID,
		Reason:      reason,
		RequestedAt: time.Now(),
		ExpiresAt:   expiresAt,
		State:       store.StatePending,
	}
	if err := b.store.Set(ctx, d, staleDuration); err != nil {
		b.log.Error("ecr push: store deploy", zap.Error(err))
	}

	// slackChannel/slackTS are written by the approval-post goroutine and
	// read after wg.Wait. The wait group provides happens-before, so the
	// read is race-free. Captured here so we can persist the message handle
	// on the PendingDeploy for later correlation by ArgoCD lifecycle events.
	var slackChannel, slackTS string

	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		// Apply deploy labels in parallel with the comment, slack post, and
		// audit log so the label REST round trip does not extend the
		// user-visible deploy latency.
		b.warnIfErr("github: add labels", b.gh.AddLabels(ctx, prNumber, []string{cfg.DeployLabel(), cfg.PendingLabel()}), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.warnIfErr("github: comment requested", b.gh.CommentRequested(ctx, prNumber, ecrRequesterName, evt.App, evt.Tag, reason), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		// Post approval request to the appropriate channel.
		slackChannel, slackTS = b.postECRApprovalRequest(ctx, cfg, pendingInfo{
			App:         evt.App,
			Environment: env,
			Tag:         evt.Tag,
			PRNumber:    prNumber,
			PRURL:       prURL,
			RequesterID: ecrRequesterID,
			Reason:      reason,
		})
	}()
	go func() {
		defer wg.Done()
		if err := b.auditLog.Log(ctx, audit.AuditEvent{
			EventType:   audit.EventRequested,
			Trigger:     audit.TriggerECRPush,
			App:         evt.App,
			Environment: env,
			Tag:         evt.Tag,
			PRNumber:    prNumber,
			PRURL:       prURL,
			Reason:      reason,
		}); err != nil {
			b.log.Error("audit log", zap.Error(err))
		}
	}()
	b.metrics.RecordDeploy(evt.App, audit.EventRequested)
	wg.Wait()

	if slackTS != "" {
		d.SlackChannel = slackChannel
		d.SlackMessageTS = slackTS
		if err := b.store.Set(ctx, d, staleDuration); err != nil {
			b.log.Warn("store: update deploy with slack handle", zap.Error(err))
		}
	}

	b.updatePendingGauge(ctx)
	b.log.Info("ecr push: approval requested", zap.String("app", evt.App), zap.String("tag", evt.Tag), zap.Int("pr", prNumber))
}

// postECRApprovalRequest posts an approval message to the deploy channel and
// returns the (channel, message timestamp) pair so the caller can persist it
// on the PendingDeploy for later correlation.
func (b *Bot) postECRApprovalRequest(ctx context.Context, cfg *config.Config, deploy pendingInfo) (channel, ts string) {
	text := fmt.Sprintf(
		"*ECR Deploy Request*\n\nNew image `%s:%s` detected in ECR.\n*App:* %s\n*Environment:* %s\n*PR:* <%s|#%d>",
		deploy.App, deploy.Tag, deploy.App, deploy.Environment, deploy.PRURL, deploy.PRNumber,
	)

	btnApprove := slack.NewButtonBlockElement(ActionApprove, fmt.Sprintf("%d", deploy.PRNumber),
		slack.NewTextBlockObject("plain_text", "Approve", false, false))
	btnApprove.Style = "primary"

	btnReject := slack.NewButtonBlockElement(ActionReject, fmt.Sprintf("%d", deploy.PRNumber),
		slack.NewTextBlockObject("plain_text", "Reject", false, false))
	btnReject.Style = "danger"

	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil,
			),
			slack.NewActionBlock("", btnApprove, btnReject),
		),
	}
	opts = append(opts, threadOption(b.getThreadTS(ctx, deploy.Environment))...)
	return b.postSlackWithHandle(ctx, cfg.Slack.DeployChannel, "notice", opts...)
}

// notifyECRAutoDeployFailed posts a failure notice to the deploy channel,
// closes the PR, and releases the lock.
func (b *Bot) notifyECRAutoDeployFailed(ctx context.Context, evt queue.ECRPushEvent, appCfg *config.AppConfig, cfg *config.Config, prNumber int, prURL string, failErr error) {
	env := appCfg.Environment

	msg := fmt.Sprintf(
		"Auto-deploy of *%s* (%s) `%s` failed — %v. <%s|PR #%d> has been closed.",
		evt.App, env, evt.Tag, failErr, prURL, prNumber,
	)

	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		b.warnIfErr("github: comment auto-deploy failed", b.gh.CommentAutoDeployFailed(ctx, prNumber, failErr), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.warnIfErr("github: close PR", b.gh.ClosePR(ctx, prNumber), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.errIfErr("store: release lock", b.store.ReleaseLock(ctx, env, evt.App), zap.String("env", env), zap.String("app", evt.App))
	}()
	go func() {
		defer wg.Done()
		opts := []slack.MsgOption{slack.MsgOptionText(msg, false)}
		opts = append(opts, threadOption(b.getThreadTS(ctx, env))...)
		b.postSlack(ctx, cfg.Slack.DeployChannel, "notice", opts...)
	}()
	go func() {
		defer wg.Done()
		if err := b.auditLog.Log(ctx, audit.AuditEvent{
			EventType:   audit.EventConflictFailed,
			Trigger:     audit.TriggerECRPush,
			App:         evt.App,
			Environment: env,
			Tag:         evt.Tag,
			PRNumber:    prNumber,
			PRURL:       prURL,
			Reason:      "merge conflict could not be auto-resolved",
		}); err != nil {
			b.log.Error("audit log", zap.Error(err))
		}
	}()
	wg.Wait()
}
