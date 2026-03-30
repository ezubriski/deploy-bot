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

	"github.com/yourorg/deploy-bot/internal/audit"
)

func (b *Bot) handleSlashCommand(evt *socketmode.Event, client *socketmode.Client) {
	cmd, ok := evt.Data.(slack.SlashCommand)
	if !ok {
		return
	}
	client.Ack(*evt.Request)

	ctx := context.Background()
	parts := strings.Fields(cmd.Text)

	switch {
	case len(parts) == 0:
		b.openDeployModal(ctx, cmd, "")
	case parts[0] == "status":
		b.handleStatus(ctx, cmd)
	case parts[0] == "cancel" && len(parts) >= 2:
		b.handleCancel(ctx, cmd, parts[1])
	case parts[0] == "nudge" && len(parts) >= 2:
		b.handleNudge(ctx, cmd, parts[1])
	default:
		// Treat first arg as app name
		b.openDeployModal(ctx, cmd, parts[0])
	}
}

func (b *Bot) openDeployModal(ctx context.Context, cmd slack.SlashCommand, preSelectedApp string) {
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

	staleDuration, _ := b.cfg.StaleDuration()

	// Build app options
	var appOptions []*slack.OptionBlockObject
	for _, app := range b.cfg.Apps {
		appOptions = append(appOptions, slack.NewOptionBlockObject(
			app.App,
			slack.NewTextBlockObject("plain_text", app.App, false, false),
			nil,
		))
	}

	// Build tag options for first app (or pre-selected)
	tagApp := preSelectedApp
	if tagApp == "" && len(b.cfg.Apps) > 0 {
		tagApp = b.cfg.Apps[0].App
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

	modal := buildDeployModal(appOptions, tagOptions, preSelectedApp, staleDuration.String())
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
			"• *%s* `%s` — PR <%s|#%d> — <@%s> — %s old — _%s_",
			d.App, d.Tag, d.PRURL, d.PRNumber, d.RequesterID, age, d.State,
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
	_ = b.store.Delete(ctx, prNumber)

	_ = b.auditLog.Log(ctx, audit.AuditEvent{
		EventType: audit.EventCancelled,
		App:       d.App,
		Tag:       d.Tag,
		PRNumber:  prNumber,
		PRURL:     d.PRURL,
		Requester: requesterGH,
	})

	b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Deployment #%d cancelled.", prNumber))
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
	_, _, err = b.slack.PostMessageContext(ctx, d.ApproverID,
		slack.MsgOptionText(fmt.Sprintf(
			":bell: Reminder: deployment of *%s* `%s` by <@%s> is waiting for your approval. Expires in *%s*. PR: <%s|#%d>",
			d.App, d.Tag, d.RequesterID, remaining, d.PRURL, d.PRNumber,
		), false),
	)
	if err != nil {
		b.log.Error("nudge approver", zap.Error(err))
		b.postEphemeralCommand(ctx, cmd, "Failed to send nudge.")
		return
	}

	b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Nudge sent to <@%s>.", d.ApproverID))
}

func (b *Bot) postEphemeralCommand(ctx context.Context, cmd slack.SlashCommand, text string) {
	_, err := b.slack.PostEphemeralContext(ctx, cmd.ChannelID, cmd.UserID, slack.MsgOptionText(text, false))
	if err != nil {
		b.log.Error("post ephemeral", zap.Error(err))
	}
}
