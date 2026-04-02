package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"strconv"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/audit"
	githubPkg "github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/store"
)

func (b *Bot) handleInteraction(ctx context.Context, evt socketmode.Event) {
	callback, ok := evt.Data.(slack.InteractionCallback)
	if !ok {
		return
	}

	switch callback.Type {
	case slack.InteractionTypeBlockActions:
		b.handleBlockAction(ctx, callback)
	case slack.InteractionTypeViewSubmission:
		b.handleViewSubmission(ctx, callback)
	}
}

func (b *Bot) handleBlockAction(ctx context.Context, callback slack.InteractionCallback) {
	for _, action := range callback.ActionCallback.BlockActions {
		switch action.ActionID {
		case ActionApprove:
			b.handleApprove(ctx, callback, action)
		case ActionReject:
			b.handleRejectButton(ctx, callback, action)
		}
	}
}

func (b *Bot) handleApprove(ctx context.Context, callback slack.InteractionCallback, action *slack.BlockAction) {
	prNumber, err := strconv.Atoi(action.Value)
	if err != nil {
		b.log.Error("invalid PR number in approve action", zap.String("value", action.Value))
		return
	}

	approverID := callback.User.ID
	isMember, ghLogin, err := b.validator.IsApprover(ctx, approverID)
	if err != nil {
		b.log.Error("validate approver", zap.Error(err))
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID, "Failed to validate your permissions.")
		return
	}
	if !isMember {
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID, "You are not a member of the approver team.")
		return
	}

	d, err := b.store.Get(ctx, prNumber)
	if err != nil || d == nil {
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID, fmt.Sprintf("Deployment #%d not found.", prNumber))
		return
	}

	// Replace buttons with status text
	b.replaceButtons(ctx, callback, "Approved")

	// Transition state to merging
	if err := b.store.UpdateState(ctx, prNumber, store.StateMerging); err != nil {
		b.log.Error("update state to merging", zap.Error(err))
	}

	cfg := b.cfg.Load()

	// First merge attempt
	mergeErr := b.gh.MergePR(ctx, prNumber, cfg.Deployment.MergeMethod)
	if mergeErr != nil {
		if errors.Is(mergeErr, githubPkg.ErrMergeConflict) {
			// Attempt to rebase the deploy branch onto current HEAD and retry.
			appCfg, ok := cfg.AppByName(d.App)
			if !ok {
				b.log.Error("app config not found for rebase", zap.String("app", d.App))
				_ = b.store.UpdateState(ctx, prNumber, store.StatePending)
				b.notifyConflictFailed(ctx, callback.Channel.ID, callback.User.ID, d, prNumber, approverID)
				return
			}
			baseBranch, err := b.gh.GetDefaultBranch(ctx)
			if err != nil {
				b.log.Error("get default branch for rebase", zap.Error(err))
				_ = b.store.UpdateState(ctx, prNumber, store.StatePending)
				b.notifyConflictFailed(ctx, callback.Channel.ID, callback.User.ID, d, prNumber, approverID)
				return
			}
			rebaseErr := b.gh.RebaseDeployBranch(ctx, githubPkg.CreatePRParams{
				App:           d.App,
				Environment:   d.Environment,
				Tag:           d.Tag,
				KustomizePath: appCfg.KustomizePath,
				BaseBranch:    baseBranch,
			})
			if rebaseErr != nil {
				if errors.Is(rebaseErr, githubPkg.ErrNoChange) {
					// Tag is already on the default branch; the deploy happened via
					// another path. Close this PR as a no-op.
					b.closeNoOpPR(ctx, d, prNumber)
					return
				}
				b.log.Error("rebase deploy branch", zap.Int("pr", prNumber), zap.Error(rebaseErr))
				_ = b.store.UpdateState(ctx, prNumber, store.StatePending)
				b.notifyConflictFailed(ctx, callback.Channel.ID, callback.User.ID, d, prNumber, approverID)
				return
			}

			// Give GitHub a moment to recalculate mergeability after the force-push.
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}

			// Retry merge once.
			if retryErr := b.gh.MergePR(ctx, prNumber, cfg.Deployment.MergeMethod); retryErr != nil {
				b.log.Error("merge PR after rebase", zap.Int("pr", prNumber), zap.Error(retryErr))
				_ = b.store.UpdateState(ctx, prNumber, store.StatePending)
				b.notifyConflictFailed(ctx, callback.Channel.ID, callback.User.ID, d, prNumber, approverID)
				return
			}
			// Merge succeeded after rebase — fall through to completion.
		} else {
			b.log.Error("merge PR", zap.Int("pr", prNumber), zap.Error(mergeErr))
			_ = b.store.UpdateState(ctx, prNumber, store.StatePending)
			if errors.Is(mergeErr, githubPkg.ErrRateLimited) {
				b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID, fmt.Sprintf("GitHub rate limit reached. PR #%d is still open — please try approving again in a few minutes.", prNumber))
			} else {
				b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID, fmt.Sprintf("Failed to merge PR #%d: %v", prNumber, mergeErr))
			}
			return
		}
	}

	_ = b.gh.CommentApproved(ctx, prNumber, ghLogin)
	_ = b.gh.RemoveLabel(ctx, prNumber, cfg.PendingLabel())
	_ = b.store.ReleaseLock(ctx, d.Environment, d.App)
	_ = b.store.Delete(ctx, prNumber)

	// Notify requester
	_, _, _ = b.slack.PostMessageContext(ctx, d.RequesterID,
		slack.MsgOptionText(fmt.Sprintf(
			"Your deployment of *%s* (%s) `%s` (PR #%d) was *approved* by <@%s> and is now merging.",
			d.App, d.Environment, d.Tag, prNumber, approverID,
		), false),
	)

	_ = b.auditLog.Log(ctx, audit.AuditEvent{
		EventType:   audit.EventApproved,
		App:         d.App,
		Environment: d.Environment,
		Tag:         d.Tag,
		PRNumber:    prNumber,
		PRURL:       d.PRURL,
		Requester:   d.Requester,
		Approver:    ghLogin,
	})

	b.metrics.RecordDeploy(d.App, audit.EventApproved)
	b.updatePendingGauge(ctx)
	_ = b.store.PushHistory(ctx, store.HistoryEntry{
		EventType:   audit.EventApproved,
		App:         d.App,
		Environment: d.Environment,
		Tag:         d.Tag,
		PRNumber:    prNumber,
		PRURL:       d.PRURL,
		RequesterID: d.RequesterID,
		CompletedAt: time.Now(),
	})
	b.log.Info("deployment approved", zap.Int("pr", prNumber), zap.String("approver", ghLogin))
}

// notifyConflictFailed tells the approver and the deploy channel that the
// merge failed due to an unresolvable conflict. The PR is left open and state
// is reset to pending so the approver can retry after manual resolution.
func (b *Bot) notifyConflictFailed(ctx context.Context, channelID, userID string, d *store.PendingDeploy, prNumber int, approverID string) {
	b.replyEphemeral(ctx, channelID, userID,
		fmt.Sprintf("Merge of PR #%d (`%s` `%s`) failed due to a conflict that could not be auto-resolved. "+
			"The PR is still open — please check it on GitHub and re-approve once the branch is updated.",
			prNumber, d.App, d.Tag),
	)
	deployChannel := b.cfg.Load().Slack.DeployChannel
	_, _, _ = b.slack.PostMessageContext(ctx, deployChannel,
		slack.MsgOptionText(fmt.Sprintf(
			"Merge conflict on PR #%d (`%s` `%s` `%s`) — auto-resolution failed. Approver <@%s> notified.",
			prNumber, d.App, d.Environment, d.Tag, approverID,
		), false),
	)
	b.log.Warn("merge conflict unresolvable", zap.Int("pr", prNumber), zap.String("app", d.App))
}

// closeNoOpPR closes a PR that turned out to be a no-op (tag already on the
// default branch) and notifies the deploy channel. Used when a rebase during
// conflict resolution reveals the deploy already happened via another path.
func (b *Bot) closeNoOpPR(ctx context.Context, d *store.PendingDeploy, prNumber int) {
	_ = b.gh.ClosePR(ctx, prNumber)
	_ = b.gh.RemoveLabel(ctx, prNumber, b.cfg.Load().PendingLabel())
	_ = b.store.ReleaseLock(ctx, d.Environment, d.App)
	_ = b.store.Delete(ctx, prNumber)

	msg := fmt.Sprintf("`%s` (`%s`) is already running `%s` — no changes to deploy. PR #%d closed.",
		d.App, d.Environment, d.Tag, prNumber)
	b.postNoOpNotice(ctx, d.App, msg)
	b.log.Info("deploy was no-op, PR closed", zap.Int("pr", prNumber), zap.String("app", d.App))
}

func (b *Bot) handleRejectButton(ctx context.Context, callback slack.InteractionCallback, action *slack.BlockAction) {
	prNumber, err := strconv.Atoi(action.Value)
	if err != nil {
		return
	}

	d, err := b.store.Get(ctx, prNumber)
	if err != nil || d == nil {
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID, fmt.Sprintf("Deployment #%d not found.", prNumber))
		return
	}

	modal := buildRejectModal(prNumber, d.App, d.Environment, d.Tag)
	_, err = b.slack.OpenViewContext(ctx, callback.TriggerID, modal)
	if err != nil {
		b.log.Error("open reject modal", zap.Error(err))
	}
}

func (b *Bot) handleViewSubmission(ctx context.Context, callback slack.InteractionCallback) {
	switch callback.View.CallbackID {
	case ModalCallbackDeploy:
		b.handleDeploySubmit(ctx, callback)
	case ModalCallbackReject:
		b.handleRejectSubmit(ctx, callback)
	}
}

func (b *Bot) handleDeploySubmit(ctx context.Context, callback slack.InteractionCallback) {
	values := callback.View.State.Values

	appVal := values[BlockApp][ActionApp].SelectedOption.Value
	tagVal := values[BlockTag][ActionTag].SelectedOption.Value
	manualTag := values[BlockTagManual][ActionTagManual].Value
	reason := values[BlockReason][ActionReason].Value
	approverID := values[BlockApprover][ActionApprover].SelectedUser

	// Manual tag overrides dropdown selection
	tag := tagVal
	if manualTag != "" {
		tag = manualTag
	}

	requesterID := callback.User.ID

	// Validate approver is on the team. On failure, DM the user since the
	// modal has already been closed by the time this runs asynchronously.
	isMember, _, err := b.validator.IsApprover(ctx, approverID)
	if err != nil || !isMember {
		msg := "Selected approver is not a member of the approver team."
		if err != nil {
			msg = fmt.Sprintf("Failed to validate approver: %v", err)
		}
		b.dmUser(ctx, requesterID, msg)
		return
	}

	// Validate tag
	valid, err := b.ecrCache.ValidateTag(ctx, appVal, tag)
	if err != nil || !valid {
		b.dmUser(ctx, requesterID, fmt.Sprintf("Tag `%s` is not valid for app %s. Please try again.", tag, appVal))
		return
	}

	appCfg, ok := b.cfg.Load().AppByName(appVal)
	if !ok {
		b.log.Error("app not found", zap.String("app", appVal))
		return
	}
	env := appCfg.Environment

	// Acquire per-app deploy lock scoped to environment.
	lockTTL, _ := b.cfg.Load().LockTTL()
	acquired, err := b.store.AcquireLock(ctx, env, appVal, requesterID, lockTTL)
	if err != nil {
		b.log.Error("acquire deploy lock", zap.String("app", appVal), zap.Error(err))
		b.dmUser(ctx, requesterID, "Failed to check deploy lock. Please try again.")
		return
	}
	if !acquired {
		msg := fmt.Sprintf("A deployment of *%s* (%s) is already in progress.", appVal, env)
		if existing, _ := b.store.GetByEnvApp(ctx, env, appVal); existing != nil {
			msg = fmt.Sprintf("A deployment of *%s* (%s) is already in progress: <%s|PR #%d>", appVal, env, existing.PRURL, existing.PRNumber)
		}
		b.dmUser(ctx, requesterID, msg)
		return
	}

	baseBranch, err := b.gh.GetDefaultBranch(ctx)
	if err != nil {
		b.log.Error("get default branch", zap.Error(err))
		_ = b.store.ReleaseLock(ctx, env, appVal)
		return
	}

	requesterGH, err := b.validator.SlackUserToGitHub(ctx, requesterID)
	if err != nil {
		requesterGH = callback.User.Name
	}

	cfg := b.cfg.Load()
	prNumber, prURL, err := b.gh.CreateDeployPR(ctx, githubPkg.CreatePRParams{
		App:              appVal,
		Environment:      env,
		Tag:              tag,
		KustomizePath:    appCfg.KustomizePath,
		BaseBranch:       baseBranch,
		Requester:        requesterGH,
		Reason:           reason,
		RequesterSlackID: requesterID,
		Labels:           []string{cfg.DeployLabel(), cfg.PendingLabel()},
	})
	if err != nil {
		_ = b.store.ReleaseLock(ctx, env, appVal)
		if errors.Is(err, githubPkg.ErrNoChange) {
			noopMsg := fmt.Sprintf("`%s` (`%s`) is already running `%s` — no changes to deploy. No PR created.", appVal, env, tag)
			b.postNoOpNotice(ctx, appVal, noopMsg)
			b.dmUser(ctx, requesterID, noopMsg)
			_ = b.auditLog.Log(ctx, audit.AuditEvent{
				EventType:   audit.EventNoop,
				App:         appVal,
				Environment: env,
				Tag:         tag,
				Requester:   requesterGH,
			})
			b.log.Info("deploy no-op: tag already current", zap.String("app", appVal), zap.String("tag", tag))
			return
		}
		b.log.Error("create deploy PR", zap.Error(err))
		if errors.Is(err, githubPkg.ErrRateLimited) {
			b.dmUser(ctx, requesterID, "GitHub rate limit reached. Please try again in a few minutes.")
		} else {
			b.dmUser(ctx, requesterID, fmt.Sprintf("Failed to create deployment PR: %v", err))
		}
		return
	}

	staleDuration, _ := b.cfg.Load().StaleDuration()
	expiresAt := time.Now().Add(staleDuration)

	d := &store.PendingDeploy{
		App:         appVal,
		Environment: env,
		Tag:         tag,
		PRNumber:    prNumber,
		PRURL:       prURL,
		Requester:   requesterGH,
		RequesterID: requesterID,
		ApproverID:  approverID,
		Reason:      reason,
		RequestedAt: time.Now(),
		ExpiresAt:   expiresAt,
		State:       store.StatePending,
	}
	if err := b.store.Set(ctx, d, staleDuration); err != nil {
		b.log.Error("store deploy", zap.Error(err))
	}

	_ = b.gh.CommentRequested(ctx, prNumber, requesterGH, appVal, tag, reason)

	// DM approver
	_, _, _ = b.slack.PostMessageContext(ctx, approverID,
		buildApproverMessage(pendingInfo{
			App:         appVal,
			Environment: env,
			Tag:         tag,
			PRNumber:    prNumber,
			PRURL:       prURL,
			RequesterID: requesterID,
			Reason:      reason,
		})...,
	)

	// Confirm to requester
	_, _, _ = b.slack.PostMessageContext(ctx, requesterID,
		slack.MsgOptionText(fmt.Sprintf(
			"Deployment request for *%s* (%s) `%s` submitted. PR: <%s|#%d>. Waiting for approval from <@%s>.",
			appVal, env, tag, prURL, prNumber, approverID,
		), false),
	)

	_ = b.auditLog.Log(ctx, audit.AuditEvent{
		EventType:   audit.EventRequested,
		App:         appVal,
		Environment: env,
		Tag:         tag,
		PRNumber:    prNumber,
		PRURL:       prURL,
		Requester:   requesterGH,
		Reason:      reason,
	})

	b.metrics.RecordDeploy(appVal, audit.EventRequested)
	b.updatePendingGauge(ctx)
	b.log.Info("deployment requested", zap.String("app", appVal), zap.String("tag", tag), zap.Int("pr", prNumber))
}

// postNoOpNotice posts a no-op notification to the appropriate Slack target.
// If the app has an AutoDeployApproverGroup configured:
//   - Group ID (S…): posts to deploy_channel with a <!subteam^S…> mention
//   - Channel ID (C…): posts directly to that channel
//
// Otherwise posts to deploy_channel without a mention.
func (b *Bot) postNoOpNotice(ctx context.Context, appName, msg string) {
	cfg := b.cfg.Load()
	appCfg, ok := cfg.AppByName(appName)
	if !ok {
		_, _, _ = b.slack.PostMessageContext(ctx, cfg.Slack.DeployChannel, slack.MsgOptionText(msg, false))
		return
	}
	group := appCfg.AutoDeployApproverGroup
	switch {
	case strings.HasPrefix(group, "S"):
		// User group: mention in the deploy channel.
		text := fmt.Sprintf("<!subteam^%s> %s", group, msg)
		_, _, _ = b.slack.PostMessageContext(ctx, cfg.Slack.DeployChannel, slack.MsgOptionText(text, false))
	case strings.HasPrefix(group, "C"):
		// Channel: post directly there.
		_, _, _ = b.slack.PostMessageContext(ctx, group, slack.MsgOptionText(msg, false))
	default:
		_, _, _ = b.slack.PostMessageContext(ctx, cfg.Slack.DeployChannel, slack.MsgOptionText(msg, false))
	}
}

func (b *Bot) handleRejectSubmit(ctx context.Context, callback slack.InteractionCallback) {
	prNumber, err := strconv.Atoi(callback.View.PrivateMetadata)
	if err != nil {
		return
	}

	rejReason := callback.View.State.Values[BlockRejReason][ActionRejReason].Value
	approverID := callback.User.ID

	d, err := b.store.Get(ctx, prNumber)
	if err != nil || d == nil {
		return
	}

	isMember, ghLogin, err := b.validator.IsApprover(ctx, approverID)
	if err != nil || !isMember {
		b.dmUser(ctx, approverID, "You are not a member of the approver team.")
		return
	}

	_ = b.gh.CommentRejected(ctx, prNumber, ghLogin, rejReason)
	_ = b.gh.ClosePR(ctx, prNumber)
	_ = b.gh.RemoveLabel(ctx, prNumber, b.cfg.Load().PendingLabel())
	_ = b.store.ReleaseLock(ctx, d.Environment, d.App)
	_ = b.store.Delete(ctx, prNumber)

	// Notify requester
	_, _, _ = b.slack.PostMessageContext(ctx, d.RequesterID,
		slack.MsgOptionText(fmt.Sprintf(
			"Your deployment of *%s* (%s) `%s` (PR #%d) was *rejected* by <@%s>.\n\n*Reason:* %s",
			d.App, d.Environment, d.Tag, prNumber, approverID, rejReason,
		), false),
	)

	_ = b.auditLog.Log(ctx, audit.AuditEvent{
		EventType:   audit.EventRejected,
		App:         d.App,
		Environment: d.Environment,
		Tag:         d.Tag,
		PRNumber:    prNumber,
		PRURL:       d.PRURL,
		Requester:   d.Requester,
		Approver:    ghLogin,
		Rejection:   rejReason,
	})

	b.metrics.RecordDeploy(d.App, audit.EventRejected)
	b.updatePendingGauge(ctx)
	_ = b.store.PushHistory(ctx, store.HistoryEntry{
		EventType:   audit.EventRejected,
		App:         d.App,
		Environment: d.Environment,
		Tag:         d.Tag,
		PRNumber:    prNumber,
		PRURL:       d.PRURL,
		RequesterID: d.RequesterID,
		CompletedAt: time.Now(),
	})
	b.log.Info("deployment rejected", zap.Int("pr", prNumber), zap.String("approver", ghLogin))
}

func (b *Bot) replaceButtons(ctx context.Context, callback slack.InteractionCallback, statusText string) {
	var blocks []slack.Block
	for _, blk := range callback.Message.Blocks.BlockSet {
		if _, ok := blk.(*slack.ActionBlock); ok {
			blocks = append(blocks, slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("*%s*", statusText), false, false),
				nil, nil,
			))
		} else {
			blocks = append(blocks, blk)
		}
	}
	_, _, _, _ = b.slack.UpdateMessageContext(ctx,
		callback.Channel.ID,
		callback.Message.Timestamp,
		slack.MsgOptionBlocks(blocks...),
	)
}

// updatePendingGauge refreshes the pending deployments gauge from Redis.
func (b *Bot) updatePendingGauge(ctx context.Context) {
	deploys, err := b.store.GetAll(ctx)
	if err != nil {
		b.log.Warn("metrics: get pending deploys", zap.Error(err))
		return
	}
	b.metrics.SetPendingDeploys(len(deploys))
}

func (b *Bot) replyEphemeral(ctx context.Context, channelID, userID, text string) {
	_, err := b.slack.PostEphemeralContext(ctx, channelID, userID, slack.MsgOptionText(text, false))
	if err != nil {
		b.log.Error("post ephemeral", zap.Error(err))
	}
}

// dmUser sends a direct message to a Slack user ID. Used for async error
// feedback where no channel context is available (e.g. modal submissions).
func (b *Bot) dmUser(ctx context.Context, userID, text string) {
	_, _, err := b.slack.PostMessageContext(ctx, userID, slack.MsgOptionText(text, false))
	if err != nil {
		b.log.Error("dm user", zap.String("user", userID), zap.Error(err))
	}
}
