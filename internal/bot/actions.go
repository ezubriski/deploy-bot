package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/audit"
	githubPkg "github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/sanitize"
	"github.com/ezubriski/deploy-bot/internal/store"
	"github.com/ezubriski/deploy-bot/internal/validator"
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
	isMember, approverIdent, err := b.validator.IsApprover(ctx, approverID)
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
	deployChannel := cfg.Slack.DeployChannel

	// First merge attempt
	mergeErr := b.gh.MergePR(ctx, prNumber, cfg.Deployment.MergeMethod)
	if mergeErr != nil {
		switch {
		case errors.Is(mergeErr, githubPkg.ErrMergeConflict):
			// Attempt to rebase the deploy branch onto current HEAD and retry.
			appCfg, ok := cfg.AppByName(d.App)
			if !ok {
				b.log.Error("app config not found for rebase", zap.String("app", d.App))
				_ = b.store.UpdateState(ctx, prNumber, store.StatePending)
				b.notifyConflictFailed(ctx, d, prNumber, approverID)
				return
			}
			baseBranch, err := b.gh.GetDefaultBranch(ctx)
			if err != nil {
				b.log.Error("get default branch for rebase", zap.Error(err))
				_ = b.store.UpdateState(ctx, prNumber, store.StatePending)
				b.notifyConflictFailed(ctx, d, prNumber, approverID)
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
				b.notifyConflictFailed(ctx, d, prNumber, approverID)
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
				b.notifyConflictFailed(ctx, d, prNumber, approverID)
				return
			}
			// Merge succeeded after rebase — fall through to completion.

		case errors.Is(mergeErr, githubPkg.ErrCINotPassed):
			// CI is blocking — leave the PR open so CI can finish, then re-approve.
			b.log.Warn("merge blocked by CI", zap.Int("pr", prNumber))
			_ = b.store.UpdateState(ctx, prNumber, store.StatePending)
			_, _, _ = b.slack.PostMessageContext(ctx, deployChannel,
				slack.MsgOptionText(fmt.Sprintf(
					"<@%s> — merge of <%s|PR #%d> (*%s* %s `%s`) is blocked by a required status check. Re-approve once CI is green.",
					approverID, d.PRURL, prNumber, d.App, d.Environment, d.Tag,
				), false),
			)
			return

		case errors.Is(mergeErr, githubPkg.ErrDraftPR):
			// Shouldn't normally happen (drafts can't be selected in the modal),
			// but handle gracefully.
			b.log.Warn("merge blocked: PR is a draft", zap.Int("pr", prNumber))
			_ = b.store.UpdateState(ctx, prNumber, store.StatePending)
			_, _, _ = b.slack.PostMessageContext(ctx, deployChannel,
				slack.MsgOptionText(fmt.Sprintf(
					"%s — <%s|PR #%d> (*%s* %s `%s`) is in draft state and cannot be merged. Ask %s to mark it ready for review.",
					slackMention(approverID), d.PRURL, prNumber, d.App, d.Environment, d.Tag, slackMention(d.RequesterID),
				), false),
			)
			return

		case errors.Is(mergeErr, githubPkg.ErrHeadModified):
			// Race: head was updated between mergeability check and merge attempt.
			// A brief wait + direct retry (no rebase) is usually sufficient.
			b.log.Info("merge race: head modified, retrying", zap.Int("pr", prNumber))
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			if retryErr := b.gh.MergePR(ctx, prNumber, cfg.Deployment.MergeMethod); retryErr != nil {
				b.log.Error("merge PR after head-modified retry", zap.Int("pr", prNumber), zap.Error(retryErr))
				_ = b.store.UpdateState(ctx, prNumber, store.StatePending)
				_, _, _ = b.slack.PostMessageContext(ctx, deployChannel,
					slack.MsgOptionText(fmt.Sprintf(
						"<@%s> — <%s|PR #%d> (*%s* %s `%s`) could not be merged after a concurrent branch update. Please try approving again.",
						approverID, d.PRURL, prNumber, d.App, d.Environment, d.Tag,
					), false),
				)
				return
			}
			// Merge succeeded — fall through to completion.

		default:
			b.log.Error("merge PR", zap.Int("pr", prNumber), zap.Error(mergeErr))
			_ = b.store.UpdateState(ctx, prNumber, store.StatePending)
			var msg string
			if errors.Is(mergeErr, githubPkg.ErrRateLimited) {
				msg = fmt.Sprintf("<@%s> — GitHub rate limit reached. <%s|PR #%d> (*%s* %s `%s`) is still open — please try approving again in a few minutes.",
					approverID, d.PRURL, prNumber, d.App, d.Environment, d.Tag)
			} else {
				msg = fmt.Sprintf("<@%s> — failed to merge <%s|PR #%d> (*%s* %s `%s`): %v",
					approverID, d.PRURL, prNumber, d.App, d.Environment, d.Tag, mergeErr)
			}
			_, _, _ = b.slack.PostMessageContext(ctx, deployChannel, slack.MsgOptionText(msg, false))
			return
		}
	}

	var wg sync.WaitGroup
	wg.Add(7)
	go func() { defer wg.Done(); _ = b.gh.CommentApproved(ctx, prNumber, approverIdent.GitHubLogin) }()
	go func() { defer wg.Done(); _ = b.gh.RemoveLabel(ctx, prNumber, cfg.PendingLabel()) }()
	go func() { defer wg.Done(); _ = b.store.ReleaseLock(ctx, d.Environment, d.App) }()
	go func() { defer wg.Done(); _ = b.store.Delete(ctx, prNumber) }()
	go func() {
		defer wg.Done()
		approvedOpts := []slack.MsgOption{
			slack.MsgOptionText(fmt.Sprintf(
				"Deployment of *%s* (%s) `%s` (<%s|PR #%d>) *approved* by <@%s> — merging now.",
				d.App, d.Environment, d.Tag, d.PRURL, prNumber, approverID,
			), false),
		}
		approvedOpts = append(approvedOpts, threadOption(b.getThreadTS(ctx, d.Environment))...)
		_, _, _ = b.slack.PostMessageContext(ctx, deployChannel, approvedOpts...)
	}()
	go func() {
		defer wg.Done()
		_ = b.auditLog.Log(ctx, audit.AuditEvent{
			EventType:    audit.EventApproved,
			Trigger:      audit.TriggerSlashCommand,
			App:          d.App,
			Environment:  d.Environment,
			Tag:          d.Tag,
			PRNumber:     prNumber,
			PRURL:        d.PRURL,
			Requester:    d.Requester,
			Approver:     approverIdent.String(),
			ActorEmail:   approverIdent.Email,
			ActorSlackID: approverID,
			ActorName:    approverIdent.Name,
			MergeMethod:  cfg.Deployment.MergeMethod,
		})
	}()
	go func() {
		defer wg.Done()
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
	}()
	b.metrics.RecordDeploy(d.App, audit.EventApproved)
	wg.Wait()
	b.updatePendingGauge(ctx)
	b.log.Info("deployment approved", zap.Int("pr", prNumber), zap.String("approver", approverIdent.String()))
}

// notifyConflictFailed posts to the deploy channel that the merge failed due
// to an unresolvable conflict. The PR is left open and state is reset to
// pending so the approver can retry after manual resolution.
func (b *Bot) notifyConflictFailed(ctx context.Context, d *store.PendingDeploy, prNumber int, approverID string) {
	_, _, _ = b.slack.PostMessageContext(ctx, b.cfg.Load().Slack.DeployChannel,
		slack.MsgOptionText(fmt.Sprintf(
			"%s — merge conflict on <%s|PR #%d> (*%s* %s `%s`) could not be auto-resolved. "+
				"Please resolve the conflict on GitHub and re-approve. %s has been notified.",
			slackMention(approverID), d.PRURL, prNumber, d.App, d.Environment, d.Tag, slackMention(d.RequesterID),
		), false),
	)
	b.log.Warn("merge conflict unresolvable", zap.Int("pr", prNumber), zap.String("app", d.App))
}

// closeNoOpPR closes a PR that turned out to be a no-op (tag already on the
// default branch) and notifies the deploy channel. Used when a rebase during
// conflict resolution reveals the deploy already happened via another path.
func (b *Bot) closeNoOpPR(ctx context.Context, d *store.PendingDeploy, prNumber int) {
	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); _ = b.gh.CommentNoOp(ctx, prNumber, d.App, d.Tag) }()
	go func() { defer wg.Done(); _ = b.gh.ClosePR(ctx, prNumber) }()
	go func() { defer wg.Done(); _ = b.gh.RemoveLabel(ctx, prNumber, b.cfg.Load().PendingLabel()) }()
	go func() { defer wg.Done(); _ = b.store.ReleaseLock(ctx, d.Environment, d.App) }()
	go func() { defer wg.Done(); _ = b.store.Delete(ctx, prNumber) }()
	wg.Wait()

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
	vals := ModalValues(callback.View.State.Values)

	appVal := vals.SelectedOption(BlockApp, ActionApp)
	tagVal := vals.SelectedOption(BlockTag, ActionTag)
	manualTag := vals.Text(BlockTagManual, ActionTagManual)
	reason := vals.Text(BlockReason, ActionReason)
	approverID := vals.SelectedUser(BlockApprover, ActionApprover)

	// Manual tag overrides dropdown selection
	tag := tagVal
	if manualTag != "" {
		tag = manualTag
	}

	requesterID := callback.User.ID

	deployChannel := b.cfg.Load().Slack.DeployChannel

	// Validate approver and tag concurrently.
	var (
		isMember     bool
		approverErr  error
		tagValid     bool
		tagErr       error
		validationWg sync.WaitGroup
	)
	validationWg.Add(2)
	go func() {
		defer validationWg.Done()
		isMember, _, approverErr = b.validator.IsApprover(ctx, approverID)
	}()
	go func() {
		defer validationWg.Done()
		tagValid, tagErr = b.ecrCache.ValidateTag(ctx, appVal, tag)
	}()
	validationWg.Wait()

	if approverErr != nil || !isMember {
		msg := fmt.Sprintf("<@%s> — deploy request for *%s* `%s` failed: selected approver <@%s> is not a member of the approver team.", requesterID, appVal, tag, approverID)
		if approverErr != nil {
			msg = fmt.Sprintf("<@%s> — deploy request for *%s* `%s` failed: could not validate approver: %v", requesterID, appVal, tag, approverErr)
		}
		_, _, _ = b.slack.PostMessageContext(ctx, deployChannel, slack.MsgOptionText(msg, false))
		return
	}

	if tagErr != nil || !tagValid {
		_, _, _ = b.slack.PostMessageContext(ctx, deployChannel,
			slack.MsgOptionText(fmt.Sprintf(
				"<@%s> — deploy request for *%s* `%s` failed: tag not found in ECR. Use `/deploy tags %s` to list valid tags.",
				requesterID, appVal, tag, appVal,
			), false),
		)
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
		_, _, _ = b.slack.PostMessageContext(ctx, deployChannel,
			slack.MsgOptionText(fmt.Sprintf(
				"<@%s> — deploy request for *%s* (%s) `%s` failed: could not check deploy lock. Please try again.",
				requesterID, appVal, env, tag,
			), false),
		)
		return
	}
	if !acquired {
		msg := fmt.Sprintf("<@%s> — deploy of *%s* (%s) `%s` not started: a deployment is already in progress.", requesterID, appVal, env, tag)
		if existing, _ := b.store.GetByEnvApp(ctx, env, appVal); existing != nil {
			msg = fmt.Sprintf("<@%s> — deploy of *%s* (%s) `%s` not started: a deployment is already in progress (<%s|PR #%d>).", requesterID, appVal, env, tag, existing.PRURL, existing.PRNumber)
		}
		_, _, _ = b.slack.PostMessageContext(ctx, deployChannel, slack.MsgOptionText(msg, false))
		return
	}

	var (
		baseBranch     string
		branchErr      error
		requesterGH    string
		requesterIdent validator.Identity
		prepWg         sync.WaitGroup
	)
	prepWg.Add(2)
	go func() {
		defer prepWg.Done()
		baseBranch, branchErr = b.gh.GetDefaultBranch(ctx)
	}()
	go func() {
		defer prepWg.Done()
		var ghErr error
		requesterIdent, ghErr = b.validator.ResolveIdentity(ctx, requesterID)
		requesterGH = requesterIdent.GitHubLogin
		if ghErr != nil || requesterGH == "" {
			requesterGH = callback.User.Name
			if requesterGH == "" {
				requesterGH = requesterID
			}
		}
	}()
	prepWg.Wait()

	if branchErr != nil {
		b.log.Error("get default branch", zap.Error(branchErr))
		_ = b.store.ReleaseLock(ctx, env, appVal)
		return
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
			_ = b.auditLog.Log(ctx, audit.AuditEvent{
				EventType:    audit.EventNoop,
				Trigger:      audit.TriggerSlashCommand,
				App:          appVal,
				Environment:  env,
				Tag:          tag,
				Requester:    requesterIdent.String(),
				ActorEmail:   requesterIdent.Email,
				ActorSlackID: requesterID,
				ActorName:    requesterIdent.Name,
			})
			b.log.Info("deploy no-op: tag already current", zap.String("app", appVal), zap.String("tag", tag))
			return
		}
		b.log.Error("create deploy PR", zap.Error(err))
		var prErrMsg string
		if errors.Is(err, githubPkg.ErrRateLimited) {
			prErrMsg = fmt.Sprintf("<@%s> — deploy request for *%s* (%s) `%s` failed: GitHub rate limit reached. Please try again in a few minutes.", requesterID, appVal, env, tag)
		} else {
			prErrMsg = fmt.Sprintf("<@%s> — deploy request for *%s* (%s) `%s` failed: could not create PR: %v", requesterID, appVal, env, tag, err)
		}
		_, _, _ = b.slack.PostMessageContext(ctx, deployChannel, slack.MsgOptionText(prErrMsg, false))
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

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		_ = b.gh.CommentRequested(ctx, prNumber, requesterGH, appVal, tag, reason)
	}()
	go func() {
		defer wg.Done()
		// Post approval request to the deploy channel with Approve/Reject buttons.
		approvalOpts := buildApproverMessage(pendingInfo{
			App:         appVal,
			Environment: env,
			Tag:         tag,
			PRNumber:    prNumber,
			PRURL:       prURL,
			RequesterID: requesterID,
			ApproverID:  approverID,
			Reason:      reason,
		})
		approvalOpts = append(approvalOpts, threadOption(b.getThreadTS(ctx, env))...)
		_, _, _ = b.slack.PostMessageContext(ctx, deployChannel, approvalOpts...)
	}()
	go func() {
		defer wg.Done()
		_ = b.auditLog.Log(ctx, audit.AuditEvent{
			EventType:    audit.EventRequested,
			Trigger:      audit.TriggerSlashCommand,
			App:          appVal,
			Environment:  env,
			Tag:          tag,
			PRNumber:     prNumber,
			PRURL:        prURL,
			Requester:    requesterIdent.String(),
			Reason:       reason,
			ActorEmail:   requesterIdent.Email,
			ActorSlackID: requesterID,
			ActorName:    requesterIdent.Name,
		})
	}()
	b.metrics.RecordDeploy(appVal, audit.EventRequested)
	wg.Wait()
	b.updatePendingGauge(ctx)
	b.log.Info("deployment requested", zap.String("app", appVal), zap.String("tag", tag), zap.Int("pr", prNumber), zap.String("requester", requesterIdent.String()))
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
	group := appCfg.EffectiveApproverGroup(cfg.Slack.ApproverGroup)
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

	isMember, rejecterIdent, err := b.validator.IsApprover(ctx, approverID)
	if err != nil || !isMember {
		_, _, _ = b.slack.PostMessageContext(ctx, b.cfg.Load().Slack.DeployChannel,
			slack.MsgOptionText(fmt.Sprintf(
				"<@%s> — rejection of <%s|PR #%d> (*%s* %s `%s`) failed: not a member of the approver team.",
				approverID, d.PRURL, prNumber, d.App, d.Environment, d.Tag,
			), false),
		)
		return
	}

	cfg := b.cfg.Load()

	var wg sync.WaitGroup
	wg.Add(8)
	go func() { defer wg.Done(); _ = b.gh.CommentRejected(ctx, prNumber, rejecterIdent.GitHubLogin, rejReason) }()
	go func() { defer wg.Done(); _ = b.gh.ClosePR(ctx, prNumber) }()
	go func() { defer wg.Done(); _ = b.gh.RemoveLabel(ctx, prNumber, cfg.PendingLabel()) }()
	go func() { defer wg.Done(); _ = b.store.ReleaseLock(ctx, d.Environment, d.App) }()
	go func() { defer wg.Done(); _ = b.store.Delete(ctx, prNumber) }()
	go func() {
		defer wg.Done()
		rejectedOpts := []slack.MsgOption{
			slack.MsgOptionText(fmt.Sprintf(
				"Deployment of *%s* (%s) `%s` (<%s|PR #%d>) *rejected* by <@%s>.\n\n*Reason:* %s",
				d.App, d.Environment, d.Tag, d.PRURL, prNumber, approverID, sanitize.SlackText(rejReason, 500),
			), false),
		}
		rejectedOpts = append(rejectedOpts, threadOption(b.getThreadTS(ctx, d.Environment))...)
		_, _, _ = b.slack.PostMessageContext(ctx, cfg.Slack.DeployChannel, rejectedOpts...)
	}()
	go func() {
		defer wg.Done()
		_ = b.auditLog.Log(ctx, audit.AuditEvent{
			EventType:    audit.EventRejected,
			Trigger:      audit.TriggerSlashCommand,
			App:          d.App,
			Environment:  d.Environment,
			Tag:          d.Tag,
			PRNumber:     prNumber,
			PRURL:        d.PRURL,
			Requester:    d.Requester,
			Approver:     rejecterIdent.String(),
			Rejection:    rejReason,
			ActorEmail:   rejecterIdent.Email,
			ActorSlackID: approverID,
			ActorName:    rejecterIdent.Name,
		})
	}()
	go func() {
		defer wg.Done()
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
	}()
	b.metrics.RecordDeploy(d.App, audit.EventRejected)
	wg.Wait()
	b.updatePendingGauge(ctx)
	b.log.Info("deployment rejected", zap.Int("pr", prNumber), zap.String("approver", rejecterIdent.String()))
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
