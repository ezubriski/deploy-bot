package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/store"
)

func (b *Bot) handleSlashCommand(ctx context.Context, evt socketmode.Event) {
	cmd, ok := evt.Data.(slack.SlashCommand)
	if !ok {
		return
	}

	parts := strings.Fields(cmd.Text)

	switch {
	case len(parts) == 0:
		b.openDeployModal(ctx, cmd, "", "")
	case parts[0] == "status":
		b.handleStatus(ctx, cmd)
	case parts[0] == "history":
		appFilter := ""
		if len(parts) >= 2 {
			appFilter = parts[1]
		}
		b.handleHistory(ctx, cmd, appFilter)
	case parts[0] == "tags" && len(parts) >= 2:
		if len(parts) >= 3 {
			b.handleTagVerify(ctx, cmd, parts[1], parts[2])
		} else {
			b.handleTagList(ctx, cmd, parts[1])
		}
	case parts[0] == "cancel" && len(parts) >= 2:
		b.handleCancel(ctx, cmd, parts[1])
	case parts[0] == "nudge" && len(parts) >= 2:
		b.handleNudge(ctx, cmd, parts[1])
	case parts[0] == "rollback" && len(parts) >= 2:
		b.handleRollback(ctx, cmd, parts[1])
	default:
		// Treat first arg as app name
		b.openDeployModal(ctx, cmd, parts[0], "")
	}
}

func (b *Bot) openDeployModal(ctx context.Context, cmd slack.SlashCommand, preSelectedApp, preSelectedTag string) {
	// Validate deployer
	isMember, _, err := b.validator.IsDeployer(ctx, cmd.UserID)
	if err != nil {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Failed to validate permissions: %v", err))
		return
	}
	if !isMember {
		b.postEphemeralCommand(ctx, cmd, "You are not a member of the deployer team.")
		return
	}

	staleDuration, _ := b.cfg.Load().StaleDuration()

	// Build app options
	var appOptions []*slack.OptionBlockObject
	for _, app := range b.cfg.Load().Apps {
		appOptions = append(appOptions, slack.NewOptionBlockObject(
			app.App,
			slack.NewTextBlockObject("plain_text", app.App, false, false),
			nil,
		))
	}

	// Build tag options for first app (or pre-selected)
	tagApp := preSelectedApp
	if tagApp == "" && len(b.cfg.Load().Apps) > 0 {
		tagApp = b.cfg.Load().Apps[0].App
	}
	tags := b.ecrCache.RecentTags(tagApp)
	var tagOptions []*slack.OptionBlockObject
	for _, t := range tags {
		tagOptions = append(tagOptions, slack.NewOptionBlockObject(
			t,
			slack.NewTextBlockObject("plain_text", t, false, false),
			nil,
		))
	}

	modal := buildDeployModal(appOptions, tagOptions, preSelectedApp, preSelectedTag, staleDuration.String(), cmd.Command)
	_, err = b.slack.OpenViewContext(ctx, cmd.TriggerID, modal)
	if err != nil {
		b.log.Error("open deploy modal", zap.Error(err))
	}
}

func (b *Bot) handleStatus(ctx context.Context, cmd slack.SlashCommand) {
	deploys, err := b.store.GetAll(ctx)
	if err != nil {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Failed to fetch deployments: %v", err))
		return
	}

	if len(deploys) == 0 {
		b.postEphemeralCommand(ctx, cmd, "No pending deployments.")
		return
	}

	now := time.Now()
	var lines []string
	for _, d := range deploys {
		age := now.Sub(d.RequestedAt).Round(time.Minute)
		lines = append(lines, fmt.Sprintf(
			"• *%s* (%s) `%s` — PR <%s|#%d> — <@%s> — %s old — _%s_",
			d.App, d.Environment, d.Tag, d.PRURL, d.PRNumber, d.RequesterID, age, d.State,
		))
	}

	text := "*Pending Deployments:*\n" + strings.Join(lines, "\n")
	b.postEphemeralCommand(ctx, cmd, text)
}

func (b *Bot) handleCancel(ctx context.Context, cmd slack.SlashCommand, prArg string) {
	prNumber, err := strconv.Atoi(prArg)
	if err != nil {
		b.postEphemeralCommand(ctx, cmd, "Invalid PR number.")
		return
	}

	d, err := b.store.Get(ctx, prNumber)
	if err != nil || d == nil {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Deployment #%d not found.", prNumber))
		return
	}

	if d.RequesterID != cmd.UserID {
		b.postEphemeralCommand(ctx, cmd, "You can only cancel your own deployments.")
		return
	}

	requesterGH, err := b.validator.SlackUserToGitHub(ctx, cmd.UserID)
	if err != nil {
		requesterGH = cmd.UserName
	}

	_ = b.gh.CommentCancelled(ctx, prNumber, requesterGH)
	_ = b.gh.ClosePR(ctx, prNumber)
	_ = b.gh.RemoveLabel(ctx, prNumber, b.cfg.Load().PendingLabel())
	_ = b.store.ReleaseLock(ctx, d.Environment, d.App)
	_ = b.store.Delete(ctx, prNumber)

	_ = b.auditLog.Log(ctx, audit.AuditEvent{
		EventType:   audit.EventCancelled,
		App:         d.App,
		Environment: d.Environment,
		Tag:         d.Tag,
		PRNumber:    prNumber,
		PRURL:       d.PRURL,
		Requester:   requesterGH,
	})

	b.metrics.RecordDeploy(d.App, audit.EventCancelled)
	b.updatePendingGauge(ctx)
	_ = b.store.PushHistory(ctx, store.HistoryEntry{
		EventType:   audit.EventCancelled,
		App:         d.App,
		Environment: d.Environment,
		Tag:         d.Tag,
		PRNumber:    prNumber,
		PRURL:       d.PRURL,
		RequesterID: d.RequesterID,
		CompletedAt: time.Now(),
	})
	_, _, _ = b.slack.PostMessageContext(ctx, b.cfg.Load().Slack.DeployChannel,
		slack.MsgOptionText(fmt.Sprintf(
			"Deployment of *%s* (%s) `%s` (<%s|PR #%d>) *cancelled* by <@%s>.",
			d.App, d.Environment, d.Tag, d.PRURL, prNumber, cmd.UserID,
		), false),
	)
	b.log.Info("deployment cancelled", zap.Int("pr", prNumber), zap.String("user", cmd.UserName))
}

func (b *Bot) handleNudge(ctx context.Context, cmd slack.SlashCommand, prArg string) {
	prNumber, err := strconv.Atoi(prArg)
	if err != nil {
		b.postEphemeralCommand(ctx, cmd, "Invalid PR number.")
		return
	}

	d, err := b.store.Get(ctx, prNumber)
	if err != nil || d == nil {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Deployment #%d not found.", prNumber))
		return
	}

	remaining := time.Until(d.ExpiresAt).Round(time.Minute)
	_, _, err = b.slack.PostMessageContext(ctx, b.cfg.Load().Slack.DeployChannel,
		slack.MsgOptionText(fmt.Sprintf(
			":bell: <@%s> — reminder: deployment of *%s* (%s) `%s` by <@%s> is waiting for your approval. Expires in *%s*. <%s|PR #%d>",
			d.ApproverID, d.App, d.Environment, d.Tag, d.RequesterID, remaining, d.PRURL, d.PRNumber,
		), false),
	)
	if err != nil {
		b.log.Error("nudge approver", zap.Error(err))
	}
}

func (b *Bot) handleHistory(ctx context.Context, cmd slack.SlashCommand, appFilter string) {
	const defaultLimit = 20
	const filteredLimit = 100

	limit := defaultLimit
	if appFilter != "" {
		limit = filteredLimit
	}

	entries, err := b.store.GetHistory(ctx, limit)
	if err != nil {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Failed to fetch history: %v", err))
		return
	}

	if appFilter != "" {
		var filtered []store.HistoryEntry
		for _, e := range entries {
			if e.App == appFilter {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	if len(entries) == 0 {
		msg := "No deployment history."
		if appFilter != "" {
			msg = fmt.Sprintf("No deployment history for *%s*.", appFilter)
		}
		b.postEphemeralCommand(ctx, cmd, msg)
		return
	}

	now := time.Now()
	var lines []string
	for _, e := range entries {
		age := now.Sub(e.CompletedAt).Round(time.Minute)
		icon := eventIcon(e.EventType)
		lines = append(lines, fmt.Sprintf(
			"%s *%s* (%s) `%s` — <%s|#%d> — <@%s> — %s ago",
			icon, e.App, e.Environment, e.Tag, e.PRURL, e.PRNumber, e.RequesterID, age,
		))
	}

	header := "*Recent Deployments:*"
	if appFilter != "" {
		header = fmt.Sprintf("*Deployments for %s:*", appFilter)
	}
	b.postEphemeralCommand(ctx, cmd, header+"\n"+strings.Join(lines, "\n"))
}

// findRollbackTag scans entries (newest-first) for the two most recent approved
// deploys of appName. It returns (currentTag, previousTag, true) when found,
// and ("", "", false) when fewer than two approved entries exist.
func findRollbackTag(entries []store.HistoryEntry, appName string) (current, previous string, ok bool) {
	var approved []store.HistoryEntry
	for _, e := range entries {
		if e.App == appName && e.EventType == "approved" {
			approved = append(approved, e)
			if len(approved) == 2 {
				return approved[0].Tag, approved[1].Tag, true
			}
		}
	}
	return "", "", false
}

func eventIcon(eventType string) string {
	switch eventType {
	case "approved":
		return ":white_check_mark:"
	case "rejected":
		return ":x:"
	case "expired":
		return ":hourglass_flowing_sand:"
	case "cancelled":
		return ":no_entry_sign:"
	default:
		return ":grey_question:"
	}
}

func (b *Bot) handleRollback(ctx context.Context, cmd slack.SlashCommand, appName string) {
	isMember, _, err := b.validator.IsDeployer(ctx, cmd.UserID)
	if err != nil {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Failed to validate permissions: %v", err))
		return
	}
	if !isMember {
		b.postEphemeralCommand(ctx, cmd, "You are not a member of the deployer team.")
		return
	}

	entries, err := b.store.GetHistory(ctx, 100)
	if err != nil {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Failed to fetch history: %v", err))
		return
	}

	current, rollbackTag, ok := findRollbackTag(entries, appName)
	if !ok {
		// Distinguish zero vs one approved deploy for a clearer message.
		var count int
		var onlyTag string
		for _, e := range entries {
			if e.App == appName && e.EventType == "approved" {
				count++
				onlyTag = e.Tag
			}
		}
		if count == 0 {
			b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("No approved deployments found for *%s*.", appName))
		} else {
			b.postEphemeralCommand(ctx, cmd, fmt.Sprintf(
				"Only one approved deployment found for *%s* (`%s`). Nothing to roll back to.",
				appName, onlyTag,
			))
		}
		return
	}
	b.postEphemeralCommand(ctx, cmd, fmt.Sprintf(
		":rewind: Rolling back *%s* from `%s` to `%s`. Opening deploy modal…",
		appName, current, rollbackTag,
	))
	b.openDeployModal(ctx, cmd, appName, rollbackTag)
}

func (b *Bot) handleTagList(ctx context.Context, cmd slack.SlashCommand, appName string) {
	if _, ok := b.cfg.Load().AppByName(appName); !ok {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Unknown app *%s*.", appName))
		return
	}
	tags := b.ecrCache.Tags(appName, 20)
	if len(tags) == 0 {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("No tags found for *%s* (cache may still be warming up).", appName))
		return
	}
	lines := make([]string, len(tags))
	for i, t := range tags {
		lines[i] = fmt.Sprintf("• `%s`", t)
	}
	b.postEphemeralCommand(ctx, cmd,
		fmt.Sprintf("*Recent tags for %s:*\n%s", appName, strings.Join(lines, "\n")),
	)
}

func (b *Bot) handleTagVerify(ctx context.Context, cmd slack.SlashCommand, appName, tag string) {
	if _, ok := b.cfg.Load().AppByName(appName); !ok {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Unknown app *%s*.", appName))
		return
	}
	valid, err := b.ecrCache.ValidateTag(ctx, appName, tag)
	if err != nil {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Error checking tag: %v", err))
		return
	}
	if valid {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf(":white_check_mark: Tag `%s` is valid for *%s*.", tag, appName))
	} else {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf(":x: Tag `%s` was not found for *%s*.", tag, appName))
	}
}

func (b *Bot) postEphemeralCommand(ctx context.Context, cmd slack.SlashCommand, text string) {
	_, err := b.slack.PostEphemeralContext(ctx, cmd.ChannelID, cmd.UserID, slack.MsgOptionText(text, false))
	if err != nil {
		b.log.Error("post ephemeral", zap.Error(err))
	}
}
