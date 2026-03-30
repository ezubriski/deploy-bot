package bot

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/yourorg/deploy-bot/internal/audit"
	githubPkg "github.com/yourorg/deploy-bot/internal/github"
	"github.com/yourorg/deploy-bot/internal/store"
)

func (b *Bot) handleInteraction(evt *socketmode.Event, client *socketmode.Client) {
	callback, ok := evt.Data.(slack.InteractionCallback)
	if !ok {
		return
	}

	switch callback.Type {
	case slack.InteractionTypeBlockActions:
		client.Ack(*evt.Request)
		b.handleBlockAction(context.Background(), callback, client)
	case slack.InteractionTypeViewSubmission:
		// Ack is deferred to submission handlers so they can return validation errors.
		b.handleViewSubmission(context.Background(), evt, callback, client)
	default:
		client.Ack(*evt.Request)
	}
}

func (b *Bot) handleBlockAction(ctx context.Context, callback slack.InteractionCallback, client *socketmode.Client) {
	for _, action := range callback.ActionCallback.BlockActions {
		switch action.ActionID {
		case actionApprove:
			b.handleApprove(ctx, callback, action, client)
		case actionReject:
			b.handleRejectButton(ctx, callback, action, client)
		}
	}
}

func (b *Bot) handleApprove(ctx context.Context, callback slack.InteractionCallback, action *slack.BlockAction, client *socketmode.Client) {
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

	// Merge PR
	if err := b.gh.MergePR(ctx, prNumber, b.cfg.Deployment.MergeMethod); err != nil {
		b.log.Error("merge PR", zap.Int("pr", prNumber), zap.Error(err))
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID, fmt.Sprintf("Failed to merge PR #%d: %v", prNumber, err))
		_ = b.store.UpdateState(ctx, prNumber, store.StatePending)
		return
	}

	_ = b.gh.CommentApproved(ctx, prNumber, ghLogin)
	_ = b.store.Delete(ctx, prNumber)

	// Notify requester
	_, _, _ = b.slack.PostMessageContext(ctx, d.RequesterID,
		slack.MsgOptionText(fmt.Sprintf(
			"Your deployment of *%s* `%s` (PR #%d) was *approved* by <@%s> and is now merging.",
			d.App, d.Tag, prNumber, approverID,
		), false),
	)

	_ = b.auditLog.Log(ctx, audit.AuditEvent{
		EventType: audit.EventApproved,
		App:       d.App,
		Tag:       d.Tag,
		PRNumber:  prNumber,
		PRURL:     d.PRURL,
		Requester: d.Requester,
		Approver:  ghLogin,
	})

	b.metrics.RecordDeploy(d.App, audit.EventApproved)
	b.updatePendingGauge(ctx)
	b.log.Info("deployment approved", zap.Int("pr", prNumber), zap.String("approver", ghLogin))
}

func (b *Bot) handleRejectButton(ctx context.Context, callback slack.InteractionCallback, action *slack.BlockAction, client *socketmode.Client) {
	prNumber, err := strconv.Atoi(action.Value)
	if err != nil {
		return
	}

	d, err := b.store.Get(ctx, prNumber)
	if err != nil || d == nil {
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID, fmt.Sprintf("Deployment #%d not found.", prNumber))
		return
	}

	modal := buildRejectModal(prNumber, d.App, d.Tag)
	_, err = b.slack.OpenViewContext(ctx, callback.TriggerID, modal)
	if err != nil {
		b.log.Error("open reject modal", zap.Error(err))
	}
}

func (b *Bot) handleViewSubmission(ctx context.Context, evt *socketmode.Event, callback slack.InteractionCallback, client *socketmode.Client) {
	switch callback.View.CallbackID {
	case modalCallbackDeploy:
		b.handleDeploySubmit(ctx, evt, callback, client)
	case modalCallbackReject:
		client.Ack(*evt.Request)
		b.handleRejectSubmit(ctx, callback)
	default:
		client.Ack(*evt.Request)
	}
}

func (b *Bot) handleDeploySubmit(ctx context.Context, evt *socketmode.Event, callback slack.InteractionCallback, client *socketmode.Client) {
	values := callback.View.State.Values

	appVal := values[blockApp][actionApp].SelectedOption.Value
	tagVal := values[blockTag][actionTag].SelectedOption.Value
	manualTag := values[blockTagManual][actionTagManual].Value
	reason := values[blockReason][actionReason].Value
	approverID := values[blockApprover][actionApprover].SelectedUser

	// Manual tag overrides dropdown selection
	tag := tagVal
	if manualTag != "" {
		tag = manualTag
	}

	requesterID := callback.User.ID

	// Validate approver is on the team
	isMember, _, err := b.validator.IsApprover(ctx, approverID)
	if err != nil || !isMember {
		msg := "Selected approver is not a member of the approver team."
		if err != nil {
			msg = fmt.Sprintf("Failed to validate approver: %v", err)
		}
		client.Ack(*evt.Request, map[string]interface{}{
			"response_action": "errors",
			"errors":          map[string]string{blockApprover: msg},
		})
		return
	}

	// Validate tag
	valid, err := b.ecrCache.ValidateTag(ctx, appVal, tag)
	if err != nil || !valid {
		errMsg := fmt.Sprintf("Tag `%s` is not valid for app %s.", tag, appVal)
		client.Ack(*evt.Request, map[string]interface{}{
			"response_action": "errors",
			"errors":          map[string]string{blockTagManual: errMsg},
		})
		return
	}

	// All validation passed — ack and proceed
	client.Ack(*evt.Request)

	appCfg, ok := b.cfg.AppByName(appVal)
	if !ok {
		b.log.Error("app not found", zap.String("app", appVal))
		return
	}

	baseBranch, err := b.gh.GetDefaultBranch(ctx)
	if err != nil {
		b.log.Error("get default branch", zap.Error(err))
		return
	}

	requesterGH, err := b.validator.SlackUserToGitHub(ctx, requesterID)
	if err != nil {
		requesterGH = callback.User.Name
	}

	prNumber, prURL, err := b.gh.CreateDeployPR(ctx, githubPkg.CreatePRParams{
		App:           appVal,
		Tag:           tag,
		KustomizePath: appCfg.KustomizePath,
		BaseBranch:    baseBranch,
		Requester:     requesterGH,
		Reason:        reason,
	})
	if err != nil {
		b.log.Error("create deploy PR", zap.Error(err))
		_, _, _ = b.slack.PostMessageContext(ctx, requesterID,
			slack.MsgOptionText(fmt.Sprintf("Failed to create deployment PR: %v", err), false),
		)
		return
	}

	staleDuration, _ := b.cfg.StaleDuration()
	expiresAt := time.Now().Add(staleDuration)

	d := &store.PendingDeploy{
		App:         appVal,
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
			"Deployment request for *%s* `%s` submitted. PR: <%s|#%d>. Waiting for approval from <@%s>.",
			appVal, tag, prURL, prNumber, approverID,
		), false),
	)

	_ = b.auditLog.Log(ctx, audit.AuditEvent{
		EventType: audit.EventRequested,
		App:       appVal,
		Tag:       tag,
		PRNumber:  prNumber,
		PRURL:     prURL,
		Requester: requesterGH,
		Reason:    reason,
	})

	b.metrics.RecordDeploy(appVal, audit.EventRequested)
	b.updatePendingGauge(ctx)
	b.log.Info("deployment requested", zap.String("app", appVal), zap.String("tag", tag), zap.Int("pr", prNumber))
}

func (b *Bot) handleRejectSubmit(ctx context.Context, callback slack.InteractionCallback) {
	prNumber, err := strconv.Atoi(callback.View.PrivateMetadata)
	if err != nil {
		return
	}

	rejReason := callback.View.State.Values[blockRejReason][actionRejReason].Value
	approverID := callback.User.ID

	d, err := b.store.Get(ctx, prNumber)
	if err != nil || d == nil {
		return
	}

	isMember, ghLogin, err := b.validator.IsApprover(ctx, approverID)
	if err != nil || !isMember {
		b.replyEphemeral(ctx, "", approverID, "You are not a member of the approver team.")
		return
	}

	_ = b.gh.CommentRejected(ctx, prNumber, ghLogin, rejReason)
	_ = b.gh.ClosePR(ctx, prNumber)
	_ = b.store.Delete(ctx, prNumber)

	// Notify requester
	_, _, _ = b.slack.PostMessageContext(ctx, d.RequesterID,
		slack.MsgOptionText(fmt.Sprintf(
			"Your deployment of *%s* `%s` (PR #%d) was *rejected* by <@%s>.\n\n*Reason:* %s",
			d.App, d.Tag, prNumber, approverID, rejReason,
		), false),
	)

	_ = b.auditLog.Log(ctx, audit.AuditEvent{
		EventType: audit.EventRejected,
		App:       d.App,
		Tag:       d.Tag,
		PRNumber:  prNumber,
		PRURL:     d.PRURL,
		Requester: d.Requester,
		Approver:  ghLogin,
		Rejection: rejReason,
	})

	b.metrics.RecordDeploy(d.App, audit.EventRejected)
	b.updatePendingGauge(ctx)
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
