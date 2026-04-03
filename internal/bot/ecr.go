package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	lockTTL, _ := cfg.LockTTL()
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
		_ = b.store.ReleaseLock(ctx, env, evt.App)
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
		Labels:        []string{cfg.DeployLabel(), cfg.PendingLabel()},
	})
	if err != nil {
		_ = b.store.ReleaseLock(ctx, env, evt.App)
		if errors.Is(err, githubPkg.ErrNoChange) {
			noopMsg := fmt.Sprintf("`%s` (`%s`) is already running `%s` — no changes to deploy (ECR push). No PR created.", evt.App, env, evt.Tag)
			b.postNoOpNotice(ctx, evt.App, noopMsg)
			_ = b.auditLog.Log(ctx, audit.AuditEvent{
				EventType:   audit.EventNoop,
				App:         evt.App,
				Environment: env,
				Tag:         evt.Tag,
				Requester:   ecrRequesterName,
			})
			b.log.Info("ecr push no-op: tag already current", zap.String("app", evt.App), zap.String("tag", evt.Tag))
			return
		}
		b.log.Error("ecr push: create deploy PR", zap.String("app", evt.App), zap.Error(err))
		_, _, _ = b.slack.PostMessageContext(ctx, deployChannel,
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
func (b *Bot) handleECRAutoDeploy(ctx context.Context, evt queue.ECRPushEvent, appCfg *config.AppConfig, cfg *config.Config, prNumber int, prURL string) {
	env := appCfg.Environment
	deployChannel := cfg.Slack.DeployChannel

	mergeErr := b.gh.MergePR(ctx, prNumber, cfg.Deployment.MergeMethod)
	if mergeErr != nil {
		// On merge failure, fall back to approval-required path.
		b.log.Error("ecr auto-deploy: merge failed, falling back to approval",
			zap.Int("pr", prNumber), zap.Error(mergeErr))
		b.handleECRApprovalRequest(ctx, evt, appCfg, cfg, prNumber, prURL)
		return
	}

	_ = b.gh.CommentApproved(ctx, prNumber, ecrRequesterName)
	_ = b.gh.RemoveLabel(ctx, prNumber, cfg.PendingLabel())
	_ = b.store.ReleaseLock(ctx, env, evt.App)

	_, _, _ = b.slack.PostMessageContext(ctx, deployChannel,
		slack.MsgOptionText(fmt.Sprintf(
			"Auto-deployed *%s* (%s) `%s` (ECR push). <%s|PR #%d> merged.",
			evt.App, env, evt.Tag, prURL, prNumber,
		), false),
	)

	_ = b.auditLog.Log(ctx, audit.AuditEvent{
		EventType:   audit.EventApproved,
		App:         evt.App,
		Environment: env,
		Tag:         evt.Tag,
		PRNumber:    prNumber,
		PRURL:       prURL,
		Requester:   ecrRequesterName,
		Approver:    ecrRequesterName,
	})

	b.metrics.RecordDeploy(evt.App, audit.EventApproved)
	b.updatePendingGauge(ctx)
	_ = b.store.PushHistory(ctx, store.HistoryEntry{
		EventType:   audit.EventApproved,
		App:         evt.App,
		Environment: env,
		Tag:         evt.Tag,
		PRNumber:    prNumber,
		PRURL:       prURL,
		RequesterID: ecrRequesterID,
		CompletedAt: time.Now(),
	})
	b.log.Info("ecr auto-deploy complete", zap.String("app", evt.App), zap.String("tag", evt.Tag), zap.Int("pr", prNumber))
}

// handleECRApprovalRequest stores the pending deploy and posts an approval
// request with Approve/Reject buttons.
func (b *Bot) handleECRApprovalRequest(ctx context.Context, evt queue.ECRPushEvent, appCfg *config.AppConfig, cfg *config.Config, prNumber int, prURL string) {
	env := appCfg.Environment
	reason := fmt.Sprintf("ECR push: %s:%s", evt.Repository, evt.Tag)

	staleDuration, _ := cfg.StaleDuration()
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

	_ = b.gh.CommentRequested(ctx, prNumber, ecrRequesterName, evt.App, evt.Tag, reason)

	// Post approval request to the appropriate channel.
	b.postECRApprovalRequest(ctx, appCfg, cfg, pendingInfo{
		App:         evt.App,
		Environment: env,
		Tag:         evt.Tag,
		PRNumber:    prNumber,
		PRURL:       prURL,
		RequesterID: ecrRequesterID,
		Reason:      reason,
	})

	_ = b.auditLog.Log(ctx, audit.AuditEvent{
		EventType:   audit.EventRequested,
		App:         evt.App,
		Environment: env,
		Tag:         evt.Tag,
		PRNumber:    prNumber,
		PRURL:       prURL,
		Requester:   ecrRequesterName,
		Reason:      reason,
	})

	b.metrics.RecordDeploy(evt.App, audit.EventRequested)
	b.updatePendingGauge(ctx)
	b.log.Info("ecr push: approval requested", zap.String("app", evt.App), zap.String("tag", evt.Tag), zap.Int("pr", prNumber))
}

// postECRApprovalRequest posts an approval message to the appropriate target
// based on auto_deploy_approver_group config.
func (b *Bot) postECRApprovalRequest(ctx context.Context, appCfg *config.AppConfig, cfg *config.Config, deploy pendingInfo) {
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

	blocks := []slack.MsgOption{
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil,
			),
			slack.NewActionBlock("", btnApprove, btnReject),
		),
	}

	group := appCfg.AutoDeployApproverGroup
	switch {
	case strings.HasPrefix(group, "S"):
		// User group: mention in the deploy channel.
		mention := fmt.Sprintf("<!subteam^%s>", group)
		blocks = append(blocks, slack.MsgOptionText(mention+" "+text, false))
		_, _, _ = b.slack.PostMessageContext(ctx, cfg.Slack.DeployChannel, blocks...)
	case strings.HasPrefix(group, "C"):
		// Channel: post directly there.
		_, _, _ = b.slack.PostMessageContext(ctx, group, blocks...)
	default:
		_, _, _ = b.slack.PostMessageContext(ctx, cfg.Slack.DeployChannel, blocks...)
	}
}
