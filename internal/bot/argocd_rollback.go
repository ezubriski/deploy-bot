package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/store"
)

// argocdRollbackPayload is the JSON-encoded Value carried on both the
// [Roll back] and [Dismiss] buttons of the phase 4 prompt message. Kept
// tiny (short JSON keys) so the combined payload comfortably fits under
// Slack's 2000-char button-value limit even with long tag strings.
//
// The rollback target is computed at prompt-post time and baked in here
// so that clicks remain deterministic even if history shifts between
// post and click. That matches the phase 3 convention where the failure
// message snapshots the failing deploy's state rather than re-reading
// history on every display.
type argocdRollbackPayload struct {
	App         string `json:"a"` // FullName: app-env composite
	Environment string `json:"e"`
	FailingTag  string `json:"f"`
	RollbackTag string `json:"r"`
}

func (p argocdRollbackPayload) marshal() string {
	raw, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func parseArgoCDRollbackPayload(raw string) (argocdRollbackPayload, error) {
	var p argocdRollbackPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return argocdRollbackPayload{}, err
	}
	return p, nil
}

// findPreviousApprovedBefore scans history (newest-first) for the most
// recent `approved` entry for fullName whose CompletedAt is strictly
// before the failing entry's CompletedAt. Returns (nil, false) if no
// prior approved deploy exists — callers must suppress the rollback
// prompt in that case, because there is nothing to roll back to.
//
// Using strictly-before (rather than findRollbackEntries' "pick the
// two most recent") handles the race where a newer deploy has already
// completed by the time the ArgoCD notification arrives: we still want
// to roll back past the failing deploy, not past an unrelated newer
// one.
func findPreviousApprovedBefore(entries []store.HistoryEntry, fullName string, before time.Time) (*store.HistoryEntry, bool) {
	for i := range entries {
		e := entries[i]
		if e.App != fullName || e.EventType != "approved" {
			continue
		}
		if !e.CompletedAt.Before(before) {
			// Ignore the failing entry itself and any newer ones.
			continue
		}
		return &e, true
	}
	return nil, false
}

// postArgoCDRollbackPrompt is the phase 4 "suggest an action" message.
// It is posted as a separate top-level message alongside (not inside)
// the alarming failure status message so the buttons read as a
// discrete, review-once interaction rather than getting buried in the
// failing-resources list.
//
// The prompt is suppressed in two cases:
//
//  1. Late arrival (deploy > lateArrivalThreshold old). A hours-old
//     deploy that has been healthy is almost never the right thing to
//     roll back; the status message still surfaces the failure, but
//     the suggestion to roll back is silently omitted.
//
//  2. No prior approved history entry exists for this app/env. The
//     failing deploy is the first-ever deploy of this app, so there is
//     no known-good tag to roll back to — posting a [Roll back] button
//     with nothing behind it would be worse than posting nothing.
//
// In both suppression cases the function logs the reason at info level
// so operators can verify the behaviour from logs without enabling debug.
func (b *Bot) postArgoCDRollbackPrompt(
	ctx context.Context,
	channel string,
	entry *store.HistoryEntry,
	lateArrival bool,
) {
	if lateArrival {
		b.log.Info("argocd rollback prompt suppressed: late arrival",
			zap.String("app", entry.App),
			zap.String("env", entry.Environment),
			zap.String("tag", entry.Tag),
		)
		return
	}

	history, err := b.store.GetHistory(ctx, 100)
	if err != nil {
		// History lookup failures are non-fatal: the failure status
		// message has already posted, and dropping the prompt is safer
		// than posting a [Roll back] button that can't resolve its
		// target. Operators see the warn in logs.
		b.log.Warn("argocd rollback prompt: history lookup failed, dropping prompt",
			zap.String("app", entry.App),
			zap.String("env", entry.Environment),
			zap.Error(err),
		)
		return
	}

	// entry.App is already the composite "app-env" FullName — re-
	// concatenating entry.Environment here would break history lookups
	// and render "myapp-prod-prod" in the prompt.
	fullName := entry.App
	prev, ok := findPreviousApprovedBefore(history, fullName, entry.CompletedAt)
	if !ok {
		b.log.Info("argocd rollback prompt suppressed: no prior approved deploy",
			zap.String("app", entry.App),
			zap.String("env", entry.Environment),
			zap.String("failing_tag", entry.Tag),
		)
		return
	}

	payload := argocdRollbackPayload{
		App:         fullName,
		Environment: entry.Environment,
		FailingTag:  entry.Tag,
		RollbackTag: prev.Tag,
	}.marshal()

	text := fmt.Sprintf(
		":rewind: *Suggested action:* roll back *%s* from `%s` to `%s` (last known-good, deployed %s).",
		fullName, entry.Tag, prev.Tag,
		humanizeAge(time.Since(prev.CompletedAt)),
	)

	btnRollback := slack.NewButtonBlockElement(
		ActionArgoCDRollback, payload,
		slack.NewTextBlockObject("plain_text", "Roll back", false, false),
	)
	btnRollback.Style = "primary"

	btnDismiss := slack.NewButtonBlockElement(
		ActionArgoCDDismiss, payload,
		slack.NewTextBlockObject("plain_text", "Dismiss", false, false),
	)
	btnDismiss.Style = "danger"

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
		slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn",
				"_Click Roll back to open the approval modal with the previous tag pre-filled, "+
					"or Dismiss if you're fixing forward._",
				false, false,
			),
		),
		slack.NewActionBlock("", btnRollback, btnDismiss),
	}

	// Top-level on purpose: matches the failure status message so both
	// land at the same depth in the channel, and so dismissing the
	// prompt doesn't also hide the underlying failure context.
	b.postSlack(ctx, channel, "argocd rollback prompt", slack.MsgOptionBlocks(blocks...))
}

// handleArgoCDRollbackClick is the block-action dispatch for the
// [Roll back] button on a phase 4 prompt. It validates the clicker is
// an authorized deployer (same bar as opening /deploy rollback), then
// opens the standard deploy modal in rollback mode with the app and
// previous known-good tag pre-filled. The prompt message itself is
// left intact so the context stays visible in the channel; the new
// deploy flow will enforce lock semantics if a concurrent deploy has
// already started.
func (b *Bot) handleArgoCDRollbackClick(ctx context.Context, callback slack.InteractionCallback, action *slack.BlockAction) {
	payload, err := parseArgoCDRollbackPayload(action.Value)
	if err != nil {
		b.log.Error("argocd rollback click: bad payload",
			zap.String("value", action.Value),
			zap.Error(err),
		)
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID,
			"Sorry, this rollback prompt is malformed — please use `/deploy rollback` instead.")
		return
	}

	isMember, _, err := b.validator.IsMember(ctx, callback.User.ID)
	if err != nil {
		b.log.Error("argocd rollback click: validate member", zap.Error(err))
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID,
			"Failed to validate your permissions.")
		return
	}
	if !isMember {
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID,
			"You are not a member of the authorized team.")
		return
	}

	cfg := b.cfg.Load()
	appCfg, ok := cfg.AppByName(payload.App)
	if !ok {
		// App removed from config between the failure posting and the
		// click. Tell the user rather than silently doing nothing.
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID,
			fmt.Sprintf("App *%s* is no longer in the deploy-bot config — can't open rollback modal.", payload.App))
		return
	}

	params := b.buildFilteredModalParams(ctx, cfg, appCfg.App, appCfg.Environment, payload.RollbackTag, true)
	params.StaleDuration = cfg.StaleDuration().String()
	params.CommandName = "/deploy"

	modal := buildDeployModal(params)
	if _, err := b.slack.OpenViewContext(ctx, callback.TriggerID, modal); err != nil {
		b.log.Error("argocd rollback click: open modal", zap.Error(err))
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID,
			"Failed to open the rollback modal.")
		return
	}

	b.log.Info("argocd rollback prompt: rollback modal opened",
		zap.String("app", payload.App),
		zap.String("failing_tag", payload.FailingTag),
		zap.String("rollback_tag", payload.RollbackTag),
		zap.String("user", callback.User.ID),
	)
}

// handleArgoCDDismissClick is the block-action dispatch for the
// [Dismiss] button on a phase 4 prompt. It validates the clicker is a
// member of the authorized team (same bar as approving a deploy), then
// replaces the prompt's buttons in place with a "Dismissed by @user"
// line so the prompt can't be clicked twice. The underlying failure
// status message is left untouched — Dismiss only clears the suggested
// action, not the record of what went wrong.
func (b *Bot) handleArgoCDDismissClick(ctx context.Context, callback slack.InteractionCallback, action *slack.BlockAction) {
	payload, err := parseArgoCDRollbackPayload(action.Value)
	if err != nil {
		b.log.Error("argocd dismiss click: bad payload",
			zap.String("value", action.Value),
			zap.Error(err),
		)
		return
	}

	isMember, _, err := b.validator.IsMember(ctx, callback.User.ID)
	if err != nil {
		b.log.Error("argocd dismiss click: validate member", zap.Error(err))
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID,
			"Failed to validate your permissions.")
		return
	}
	if !isMember {
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID,
			"Only members of the authorized team can dismiss this prompt.")
		return
	}

	// Rebuild the prompt with the buttons replaced by a dismissed-by
	// context line. Keep the original suggestion text so the historical
	// record of "what we would have suggested" stays visible.
	suggestion := fmt.Sprintf(
		":rewind: *Suggested action:* roll back *%s* from `%s` to `%s`.",
		payload.App, payload.FailingTag, payload.RollbackTag,
	)
	dismissedLine := fmt.Sprintf(
		":no_entry_sign: *Dismissed* by %s at %s — fixing forward.",
		slackMention(callback.User.ID),
		time.Now().UTC().Format("15:04 MST"),
	)

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", suggestion, false, false),
			nil, nil,
		),
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", dismissedLine, false, false),
			nil, nil,
		),
	}

	if _, _, _, err := b.slack.UpdateMessageContext(ctx,
		callback.Channel.ID,
		callback.Message.Timestamp,
		slack.MsgOptionBlocks(blocks...),
	); err != nil {
		b.log.Warn("argocd dismiss click: update message",
			zap.String("channel", callback.Channel.ID),
			zap.Error(err),
		)
		return
	}

	b.log.Info("argocd rollback prompt dismissed",
		zap.String("app", payload.App),
		zap.String("failing_tag", payload.FailingTag),
		zap.String("user", callback.User.ID),
	)
}
