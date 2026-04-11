package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
		case ActionAppName, ActionEnv:
			b.handleDeployFilter(ctx, callback)
			return // one view update per event
		case ActionTagManual:
			b.handleManualTagValidation(ctx, callback)
			return
		}
	}
}

// handleDeployFilter updates the deploy modal in response to an app name or
// environment dropdown change. It recomputes filtered options and calls
// UpdateViewContext to refresh the modal.
func (b *Bot) handleDeployFilter(ctx context.Context, callback slack.InteractionCallback) {
	vals := ModalValues(callback.View.State.Values)

	state := ParseDeployModalState(callback.View.PrivateMetadata)
	selectedApp := vals.SelectedOption(BlockAppName, ActionAppName)
	selectedEnv := vals.SelectedOption(BlockEnv, ActionEnv)

	cfg := b.cfg.Load()
	params := b.buildFilteredModalParams(ctx, cfg, selectedApp, selectedEnv, "", state.IsRollback)
	params.StaleDuration = cfg.StaleDuration().String()

	// Preserve user-entered values across the view update.
	params.ManualTag = vals.Text(BlockTagManual, ActionTagManual)
	params.Reason = vals.Text(BlockReason, ActionReason)
	params.Approver = vals.SelectedUser(BlockApprover, ActionApprover)

	modal := buildDeployModal(params)
	_, err := b.slack.UpdateViewContext(ctx, modal, "", callback.View.Hash, callback.View.ID)
	if err != nil {
		b.log.Warn("update deploy modal", zap.Error(err))
	}
}

// handleManualTagValidation validates a manually entered tag against the ECR
// cache and updates the modal with a validation result.
func (b *Bot) handleManualTagValidation(ctx context.Context, callback slack.InteractionCallback) {
	vals := ModalValues(callback.View.State.Values)
	state := ParseDeployModalState(callback.View.PrivateMetadata)

	selectedApp := vals.SelectedOption(BlockAppName, ActionAppName)
	selectedEnv := vals.SelectedOption(BlockEnv, ActionEnv)
	manualTag := vals.Text(BlockTagManual, ActionTagManual)

	cfg := b.cfg.Load()
	params := b.buildFilteredModalParams(ctx, cfg, selectedApp, selectedEnv, "", state.IsRollback)
	params.StaleDuration = cfg.StaleDuration().String()
	params.ManualTag = manualTag
	params.Reason = vals.Text(BlockReason, ActionReason)
	params.Approver = vals.SelectedUser(BlockApprover, ActionApprover)

	if manualTag != "" && selectedApp != "" && selectedEnv != "" {
		fullName := selectedApp + "-" + selectedEnv
		exists, err := b.ecrCache.ValidateTag(ctx, fullName, manualTag)
		if err != nil {
			b.log.Warn("validate manual tag", zap.Error(err))
		} else if exists {
			params.TagValidation = fmt.Sprintf(":white_check_mark: Tag `%s` found.", manualTag)
		} else {
			// CommandName isn't preserved across view updates (PrivateMetadata
			// only carries app/env/rollback), so hardcode /deploy here.
			params.TagValidation = fmt.Sprintf(
				":x: The tag `%s` was not found for *%s*. Run `/deploy tags %s` in this channel to list valid tags.",
				manualTag, fullName, fullName,
			)
		}
	} else if manualTag != "" {
		params.TagValidation = "_Select an app and environment to validate this tag._"
	}

	modal := buildDeployModal(params)
	_, err := b.slack.UpdateViewContext(ctx, modal, "", callback.View.Hash, callback.View.ID)
	if err != nil {
		b.log.Warn("update deploy modal (tag validation)", zap.Error(err))
	}
}

func (b *Bot) handleApprove(ctx context.Context, callback slack.InteractionCallback, action *slack.BlockAction) {
	prNumber, err := strconv.Atoi(action.Value)
	if err != nil {
		b.log.Error("invalid PR number in approve action", zap.String("value", action.Value))
		return
	}

	approverID := callback.User.ID
	isMember, approverIdent, err := b.validator.IsMember(ctx, approverID)
	if err != nil {
		b.log.Error("validate approver", zap.Error(err))
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID, "Failed to validate your permissions.")
		return
	}
	if !isMember {
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID, "You are not a member of the authorized team.")
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

	// First merge attempt. mergeSHA is captured here and on each retry path
	// so it can be persisted on the resulting HistoryEntry. Downstream
	// signals (e.g. ArgoCD notifications carrying a synced revision) use
	// this SHA to correlate back to the deploy that produced it.
	mergeSHA, mergeErr := b.gh.MergePR(ctx, prNumber, cfg.Deployment.MergeMethod)
	if mergeErr != nil {
		switch {
		case errors.Is(mergeErr, githubPkg.ErrMergeConflict):
			// Attempt to rebase the deploy branch onto current HEAD and retry.
			appCfg, ok := cfg.AppByName(d.App)
			if !ok {
				b.log.Error("app config not found for rebase", zap.String("app", d.App))
				b.warnIfErr("store: reset to pending", b.store.UpdateState(ctx, prNumber, store.StatePending), zap.Int("pr", prNumber))
				b.notifyConflictFailed(ctx, d, prNumber, approverID)
				return
			}
			baseBranch, err := b.gh.GetDefaultBranch(ctx)
			if err != nil {
				b.log.Error("get default branch for rebase", zap.Error(err))
				b.warnIfErr("store: reset to pending", b.store.UpdateState(ctx, prNumber, store.StatePending), zap.Int("pr", prNumber))
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
				b.warnIfErr("store: reset to pending", b.store.UpdateState(ctx, prNumber, store.StatePending), zap.Int("pr", prNumber))
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
			retrySHA, retryErr := b.gh.MergePR(ctx, prNumber, cfg.Deployment.MergeMethod)
			if retryErr != nil {
				b.log.Error("merge PR after rebase", zap.Int("pr", prNumber), zap.Error(retryErr))
				b.warnIfErr("store: reset to pending", b.store.UpdateState(ctx, prNumber, store.StatePending), zap.Int("pr", prNumber))
				b.notifyConflictFailed(ctx, d, prNumber, approverID)
				return
			}
			mergeSHA = retrySHA
			// Merge succeeded after rebase — fall through to completion.

		case errors.Is(mergeErr, githubPkg.ErrCINotPassed):
			// CI is blocking — leave the PR open so CI can finish, then re-approve.
			b.log.Warn("merge blocked by CI", zap.Int("pr", prNumber))
			b.warnIfErr("store: reset to pending", b.store.UpdateState(ctx, prNumber, store.StatePending), zap.Int("pr", prNumber))
			b.postSlack(ctx, deployChannel, "notice",
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
			b.warnIfErr("store: reset to pending", b.store.UpdateState(ctx, prNumber, store.StatePending), zap.Int("pr", prNumber))
			b.postSlack(ctx, deployChannel, "notice",
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
			retrySHA, retryErr := b.gh.MergePR(ctx, prNumber, cfg.Deployment.MergeMethod)
			if retryErr != nil {
				b.log.Error("merge PR after head-modified retry", zap.Int("pr", prNumber), zap.Error(retryErr))
				b.warnIfErr("store: reset to pending", b.store.UpdateState(ctx, prNumber, store.StatePending), zap.Int("pr", prNumber))
				b.postSlack(ctx, deployChannel, "notice",
					slack.MsgOptionText(fmt.Sprintf(
						"<@%s> — <%s|PR #%d> (*%s* %s `%s`) could not be merged after a concurrent branch update. Please try approving again.",
						approverID, d.PRURL, prNumber, d.App, d.Environment, d.Tag,
					), false),
				)
				return
			}
			mergeSHA = retrySHA
			// Merge succeeded — fall through to completion.

		default:
			b.log.Error("merge PR", zap.Int("pr", prNumber), zap.Error(mergeErr))
			b.warnIfErr("store: reset to pending", b.store.UpdateState(ctx, prNumber, store.StatePending), zap.Int("pr", prNumber))
			var msg string
			if errors.Is(mergeErr, githubPkg.ErrRateLimited) {
				msg = fmt.Sprintf("<@%s> — GitHub rate limit reached. <%s|PR #%d> (*%s* %s `%s`) is still open — please try approving again in a few minutes.",
					approverID, d.PRURL, prNumber, d.App, d.Environment, d.Tag)
			} else {
				msg = fmt.Sprintf("<@%s> — failed to merge <%s|PR #%d> (*%s* %s `%s`): %v",
					approverID, d.PRURL, prNumber, d.App, d.Environment, d.Tag, mergeErr)
			}
			b.postSlack(ctx, deployChannel, "approve flow notice", slack.MsgOptionText(msg, false))
			return
		}
	}

	var wg sync.WaitGroup
	wg.Add(7)
	go func() {
		defer wg.Done()
		b.warnIfErr("github: comment approved", b.gh.CommentApproved(ctx, prNumber, approverIdent.GitHubLogin), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.warnIfErr("github: remove pending label", b.gh.RemoveLabel(ctx, prNumber, cfg.PendingLabel()), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.errIfErr("store: release lock", b.store.ReleaseLock(ctx, d.Environment, d.App), zap.String("env", d.Environment), zap.String("app", d.App))
	}()
	go func() {
		defer wg.Done()
		b.errIfErr("store: delete pending", b.store.Delete(ctx, prNumber), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		approvedOpts := []slack.MsgOption{
			slack.MsgOptionText(fmt.Sprintf(
				"Deployment of *%s* (%s) `%s` (<%s|PR #%d>) *approved* by <@%s> — merging now.",
				d.App, d.Environment, d.Tag, d.PRURL, prNumber, approverID,
			), false),
		}
		approvedOpts = append(approvedOpts, threadOption(b.getThreadTS(ctx, d.Environment))...)
		b.postSlack(ctx, deployChannel, "notice", approvedOpts...)
	}()
	go func() {
		defer wg.Done()
		if err := b.auditLog.Log(ctx, audit.AuditEvent{
			EventType:   audit.EventApproved,
			Trigger:     audit.TriggerSlashCommand,
			App:         d.App,
			Environment: d.Environment,
			Tag:         d.Tag,
			PRNumber:    prNumber,
			PRURL:       d.PRURL,
			ActorEmail:  approverIdent.Email,
			ActorName:   approverIdent.Name,
		}); err != nil {
			b.log.Error("audit log", zap.Error(err))
		}
	}()
	go func() {
		defer wg.Done()
		if err := b.store.PushHistory(ctx, store.HistoryEntry{
			EventType:       audit.EventApproved,
			App:             d.App,
			Environment:     d.Environment,
			Tag:             d.Tag,
			PRNumber:        prNumber,
			PRURL:           d.PRURL,
			RequesterID:     d.RequesterID,
			CompletedAt:     time.Now(),
			GitopsCommitSHA: mergeSHA,
			SlackChannel:    d.SlackChannel,
			SlackMessageTS:  d.SlackMessageTS,
		}); err != nil {
			b.log.Warn("store: push history", zap.Error(err))
		}
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
	b.postSlack(ctx, b.cfg.Load().Slack.DeployChannel, "notice",
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
	go func() {
		defer wg.Done()
		b.warnIfErr("github: comment no-op", b.gh.CommentNoOp(ctx, prNumber, d.App, d.Tag), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.warnIfErr("github: close PR", b.gh.ClosePR(ctx, prNumber), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.warnIfErr("github: remove pending label", b.gh.RemoveLabel(ctx, prNumber, b.cfg.Load().PendingLabel()), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.errIfErr("store: release lock", b.store.ReleaseLock(ctx, d.Environment, d.App), zap.String("env", d.Environment), zap.String("app", d.App))
	}()
	go func() {
		defer wg.Done()
		b.errIfErr("store: delete pending", b.store.Delete(ctx, prNumber), zap.Int("pr", prNumber))
	}()
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

	appName := vals.SelectedOption(BlockAppName, ActionAppName)
	env := vals.SelectedOption(BlockEnv, ActionEnv)
	appVal := appName + "-" + env // reconstruct FullName
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
		isMember, _, approverErr = b.validator.IsMember(ctx, approverID)
	}()
	go func() {
		defer validationWg.Done()
		tagValid, tagErr = b.ecrCache.ValidateTag(ctx, appVal, tag)
	}()
	validationWg.Wait()

	if approverErr != nil || !isMember {
		msg := fmt.Sprintf("<@%s> — deploy request for *%s* `%s` failed: selected approver <@%s> is not a member of the authorized team.", requesterID, appVal, tag, approverID)
		if approverErr != nil {
			msg = fmt.Sprintf("<@%s> — deploy request for *%s* `%s` failed: could not validate approver: %v", requesterID, appVal, tag, approverErr)
		}
		b.postSlack(ctx, deployChannel, "approve flow notice", slack.MsgOptionText(msg, false))
		return
	}

	if tagErr != nil || !tagValid {
		b.postSlack(ctx, deployChannel, "notice",
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
	// Acquire per-app deploy lock scoped to environment.
	lockTTL := b.cfg.Load().LockTTL()
	acquired, err := b.store.AcquireLock(ctx, env, appVal, requesterID, lockTTL)
	if err != nil {
		b.log.Error("acquire deploy lock", zap.String("app", appVal), zap.Error(err))
		b.postSlack(ctx, deployChannel, "notice",
			slack.MsgOptionText(fmt.Sprintf(
				"<@%s> — deploy request for *%s* (%s) `%s` failed: could not check deploy lock. Please try again.",
				requesterID, appVal, env, tag,
			), false),
		)
		return
	}
	if !acquired {
		msg := fmt.Sprintf("<@%s> — deploy of *%s* (%s) `%s` not started: a deployment is already in progress.", requesterID, appVal, env, tag)
		if existing, err := b.store.GetByEnvApp(ctx, env, appVal); err != nil {
			b.log.Warn("store: lookup existing deploy", zap.String("env", env), zap.String("app", appVal), zap.Error(err))
		} else if existing != nil {
			msg = fmt.Sprintf("<@%s> — deploy of *%s* (%s) `%s` not started: a deployment is already in progress (<%s|PR #%d>).", requesterID, appVal, env, tag, existing.PRURL, existing.PRNumber)
		}
		b.postSlack(ctx, deployChannel, "approve flow notice", slack.MsgOptionText(msg, false))
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
		b.errIfErr("store: release lock", b.store.ReleaseLock(ctx, env, appVal), zap.String("env", env), zap.String("app", appVal))
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
	})
	if err != nil {
		b.errIfErr("store: release lock", b.store.ReleaseLock(ctx, env, appVal), zap.String("env", env), zap.String("app", appVal))
		if errors.Is(err, githubPkg.ErrNoChange) {
			noopMsg := fmt.Sprintf("`%s` (`%s`) is already running `%s` — no changes to deploy. No PR created.", appVal, env, tag)
			b.postNoOpNotice(ctx, appVal, noopMsg)
			if err := b.auditLog.Log(ctx, audit.AuditEvent{
				EventType:   audit.EventNoop,
				Trigger:     audit.TriggerSlashCommand,
				App:         appVal,
				Environment: env,
				Tag:         tag,
				ActorEmail:  requesterIdent.Email,
				ActorName:   requesterIdent.Name,
			}); err != nil {
				b.log.Error("audit log", zap.Error(err))
			}
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
		b.postSlack(ctx, deployChannel, "notice", slack.MsgOptionText(prErrMsg, false))
		return
	}

	staleDuration := b.cfg.Load().StaleDuration()
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
		b.warnIfErr("github: comment requested", b.gh.CommentRequested(ctx, prNumber, requesterGH, appVal, tag, reason), zap.Int("pr", prNumber))
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
		slackChannel, slackTS = b.postSlackWithHandle(ctx, deployChannel, "notice", approvalOpts...)
	}()
	go func() {
		defer wg.Done()
		if err := b.auditLog.Log(ctx, audit.AuditEvent{
			EventType:   audit.EventRequested,
			Trigger:     audit.TriggerSlashCommand,
			App:         appVal,
			Environment: env,
			Tag:         tag,
			PRNumber:    prNumber,
			PRURL:       prURL,
			Reason:      reason,
			ActorEmail:  requesterIdent.Email,
			ActorName:   requesterIdent.Name,
		}); err != nil {
			b.log.Error("audit log", zap.Error(err))
		}
	}()
	b.metrics.RecordDeploy(appVal, audit.EventRequested)
	wg.Wait()

	// If the approval post succeeded, write the message handle back onto
	// the PendingDeploy. Done after wg.Wait so the goroutine's writes are
	// visible. A failed Slack post is non-fatal — the deploy still proceeds
	// without a stored handle, and ArgoCD correlation falls back to the
	// per-environment thread.
	if slackTS != "" {
		d.SlackChannel = slackChannel
		d.SlackMessageTS = slackTS
		if err := b.store.Set(ctx, d, staleDuration); err != nil {
			b.log.Warn("store: update deploy with slack handle", zap.Error(err))
		}
	}

	b.updatePendingGauge(ctx)
	b.log.Info("deployment requested", zap.String("app", appVal), zap.String("tag", tag), zap.Int("pr", prNumber), zap.String("requester", requesterIdent.String()))
}

// postNoOpNotice posts a no-op notification to the deploy channel.
func (b *Bot) postNoOpNotice(ctx context.Context, appName, msg string) {
	b.postSlack(ctx, b.cfg.Load().Slack.DeployChannel, "notice", slack.MsgOptionText(msg, false))
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

	isMember, rejecterIdent, err := b.validator.IsMember(ctx, approverID)
	if err != nil || !isMember {
		b.postSlack(ctx, b.cfg.Load().Slack.DeployChannel, "notice",
			slack.MsgOptionText(fmt.Sprintf(
				"<@%s> — rejection of <%s|PR #%d> (*%s* %s `%s`) failed: not a member of the authorized team.",
				approverID, d.PRURL, prNumber, d.App, d.Environment, d.Tag,
			), false),
		)
		return
	}

	cfg := b.cfg.Load()

	var wg sync.WaitGroup
	wg.Add(8)
	go func() {
		defer wg.Done()
		b.warnIfErr("github: comment rejected", b.gh.CommentRejected(ctx, prNumber, rejecterIdent.GitHubLogin, rejReason), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.warnIfErr("github: close PR", b.gh.ClosePR(ctx, prNumber), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.warnIfErr("github: remove pending label", b.gh.RemoveLabel(ctx, prNumber, cfg.PendingLabel()), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.errIfErr("store: release lock", b.store.ReleaseLock(ctx, d.Environment, d.App), zap.String("env", d.Environment), zap.String("app", d.App))
	}()
	go func() {
		defer wg.Done()
		b.errIfErr("store: delete pending", b.store.Delete(ctx, prNumber), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		rejectedOpts := []slack.MsgOption{
			slack.MsgOptionText(fmt.Sprintf(
				"Deployment of *%s* (%s) `%s` (<%s|PR #%d>) *rejected* by <@%s>.\n\n*Reason:* %s",
				d.App, d.Environment, d.Tag, d.PRURL, prNumber, approverID, sanitize.SlackText(rejReason, 500),
			), false),
		}
		rejectedOpts = append(rejectedOpts, threadOption(b.getThreadTS(ctx, d.Environment))...)
		b.postSlack(ctx, cfg.Slack.DeployChannel, "notice", rejectedOpts...)
	}()
	go func() {
		defer wg.Done()
		if err := b.auditLog.Log(ctx, audit.AuditEvent{
			EventType:   audit.EventRejected,
			Trigger:     audit.TriggerSlashCommand,
			App:         d.App,
			Environment: d.Environment,
			Tag:         d.Tag,
			PRNumber:    prNumber,
			PRURL:       d.PRURL,
			Rejection:   rejReason,
			ActorEmail:  rejecterIdent.Email,
			ActorName:   rejecterIdent.Name,
		}); err != nil {
			b.log.Error("audit log", zap.Error(err))
		}
	}()
	go func() {
		defer wg.Done()
		if err := b.store.PushHistory(ctx, store.HistoryEntry{
			EventType:      audit.EventRejected,
			App:            d.App,
			Environment:    d.Environment,
			Tag:            d.Tag,
			PRNumber:       prNumber,
			PRURL:          d.PRURL,
			RequesterID:    d.RequesterID,
			CompletedAt:    time.Now(),
			SlackChannel:   d.SlackChannel,
			SlackMessageTS: d.SlackMessageTS,
		}); err != nil {
			b.log.Warn("store: push history", zap.Error(err))
		}
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
	if _, _, _, err := b.slack.UpdateMessageContext(ctx,
		callback.Channel.ID,
		callback.Message.Timestamp,
		slack.MsgOptionBlocks(blocks...),
	); err != nil {
		b.log.Warn("slack: update approver message", zap.String("channel", callback.Channel.ID), zap.Error(err))
	}
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
