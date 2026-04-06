package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/config"
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
	case parts[0] == "list" || parts[0] == "status":
		envFilter := ""
		appFilter := ""
		if len(parts) >= 2 {
			envFilter = parts[1]
		}
		if len(parts) >= 3 {
			appFilter = parts[2]
		}
		b.handleStatus(ctx, cmd, envFilter, appFilter)
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
	case parts[0] == "apps":
		b.handleApps(ctx, cmd)
	case parts[0] == "conflicts":
		b.handleConflicts(ctx, cmd)
	case parts[0] == "help":
		b.handleSlashHelp(ctx, cmd)
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

// handleStatus lists pending deployments, optionally filtered by environment
// and/or app name:
//
//	/deploy list              — all pending
//	/deploy list prod         — all prod deploys
//	/deploy list prod nginx   — prod deploys matching "nginx"
func (b *Bot) handleStatus(ctx context.Context, cmd slack.SlashCommand, envFilter, appFilter string) {
	deploys, err := b.store.GetAll(ctx)
	if err != nil {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Failed to fetch deployments: %v", err))
		return
	}

	// Apply filters if provided.
	if envFilter != "" || appFilter != "" {
		var filtered []*store.PendingDeploy
		for _, d := range deploys {
			if envFilter != "" && !strings.EqualFold(d.Environment, envFilter) {
				continue
			}
			if appFilter != "" && !strings.EqualFold(d.App, appFilter) {
				continue
			}
			filtered = append(filtered, d)
		}
		deploys = filtered
	}

	if len(deploys) == 0 {
		msg := "No pending deployments."
		if envFilter != "" || appFilter != "" {
			filterDesc := envFilter
			if appFilter != "" {
				filterDesc += " " + appFilter
			}
			msg = fmt.Sprintf("No pending deployments matching *%s*.", strings.TrimSpace(filterDesc))

			// Suggest similar apps if the app filter didn't match exactly.
			if appFilter != "" {
				all, _ := b.store.GetAll(ctx)
				var suggestions []string
				seen := map[string]struct{}{}
				for _, d := range all {
					if envFilter != "" && !strings.EqualFold(d.Environment, envFilter) {
						continue
					}
					if _, ok := seen[d.App]; ok {
						continue
					}
					if strings.Contains(strings.ToLower(d.App), strings.ToLower(appFilter)) {
						suggestions = append(suggestions, d.App)
						seen[d.App] = struct{}{}
					}
				}
				if len(suggestions) > 0 {
					msg += "\n\nDid you mean: " + strings.Join(suggestions, ", ")
				}
			}
		}
		b.postEphemeralCommand(ctx, cmd, msg)
		return
	}

	// Group by environment, preserving order of first appearance.
	now := time.Now()
	envOrder := []string{}
	byEnv := map[string][]string{}
	for _, d := range deploys {
		age := now.Sub(d.RequestedAt).Round(time.Minute)
		requester := slackMention(d.RequesterID)
		line := fmt.Sprintf("• *%s* `%s` — <%s|PR #%d> — %s — %s old — _%s_",
			d.App, d.Tag, d.PRURL, d.PRNumber, requester, age, d.State,
		)
		if _, ok := byEnv[d.Environment]; !ok {
			envOrder = append(envOrder, d.Environment)
		}
		byEnv[d.Environment] = append(byEnv[d.Environment], line)
	}

	var sections []string
	for _, env := range envOrder {
		lines := byEnv[env]
		header := fmt.Sprintf("*%s* (%d pending)", env, len(lines))
		sections = append(sections, header+"\n"+strings.Join(lines, "\n"))
	}

	text := "*Pending Deployments*\n\n" + strings.Join(sections, "\n\n")
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

	if d.RequesterID != cmd.UserID && d.RequesterID != "" {
		b.postEphemeralCommand(ctx, cmd, "You can only cancel your own deployments.")
		return
	}

	cancellerIdent, err := b.validator.ResolveIdentity(ctx, cmd.UserID)
	requesterGH := cancellerIdent.GitHubLogin
	if err != nil || requesterGH == "" {
		requesterGH = cmd.UserName
		if requesterGH == "" {
			requesterGH = "slack:" + cmd.UserID
		}
	}

	cfg := b.cfg.Load()

	var wg sync.WaitGroup
	wg.Add(8)
	go func() { defer wg.Done(); _ = b.gh.CommentCancelled(ctx, prNumber, requesterGH) }()
	go func() { defer wg.Done(); _ = b.gh.ClosePR(ctx, prNumber) }()
	go func() { defer wg.Done(); _ = b.gh.RemoveLabel(ctx, prNumber, cfg.PendingLabel()) }()
	go func() { defer wg.Done(); _ = b.store.ReleaseLock(ctx, d.Environment, d.App) }()
	go func() { defer wg.Done(); _ = b.store.Delete(ctx, prNumber) }()
	go func() {
		defer wg.Done()
		_ = b.auditLog.Log(ctx, audit.AuditEvent{
			EventType:    audit.EventCancelled,
			Trigger:      audit.TriggerSlashCommand,
			App:          d.App,
			Environment:  d.Environment,
			Tag:          d.Tag,
			PRNumber:     prNumber,
			PRURL:        d.PRURL,
			ActorEmail:   cancellerIdent.Email,
			ActorSlackID: cmd.UserID,
			ActorName:    cancellerIdent.Name,
		})
	}()
	go func() {
		defer wg.Done()
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
	}()
	go func() {
		defer wg.Done()
		_, _, _ = b.slack.PostMessageContext(ctx, cfg.Slack.DeployChannel,
			slack.MsgOptionText(fmt.Sprintf(
				"Deployment of *%s* (%s) `%s` (<%s|PR #%d>) *cancelled* by <@%s>.",
				d.App, d.Environment, d.Tag, d.PRURL, prNumber, cmd.UserID,
			), false),
		)
	}()
	b.metrics.RecordDeploy(d.App, audit.EventCancelled)
	wg.Wait()
	b.updatePendingGauge(ctx)
	b.log.Info("deployment cancelled", zap.Int("pr", prNumber), zap.String("user", cancellerIdent.String()))
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
	cfg := b.cfg.Load()
	approver := slackMention(d.ApproverID)
	channel := cfg.Slack.DeployChannel
	// If no specific approver (ECR deploys), mention the approver group.
	if d.ApproverID == "" {
		if appCfg, ok := cfg.AppByName(d.App); ok {
			group := appCfg.EffectiveApproverGroup(cfg.Slack.ApproverGroup)
			if strings.HasPrefix(group, "S") {
				approver = fmt.Sprintf("<!subteam^%s>", group)
			} else if strings.HasPrefix(group, "C") {
				approver = "approvers"
				channel = group // post to the approver channel directly
			}
		} else {
			approver = "approver team"
		}
	}
	_, _, err = b.slack.PostMessageContext(ctx, channel,
		slack.MsgOptionText(fmt.Sprintf(
			":bell: %s — reminder: deployment of *%s* (%s) `%s` by %s is waiting for approval. Expires in *%s*. <%s|PR #%d>",
			approver, d.App, d.Environment, d.Tag, slackMention(d.RequesterID), remaining, d.PRURL, d.PRNumber,
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
			"%s *%s* (%s) `%s` — <%s|#%d> — %s — %s ago",
			icon, e.App, e.Environment, e.Tag, e.PRURL, e.PRNumber, slackMention(e.RequesterID), age,
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
		b.postEphemeralCommand(ctx, cmd, b.unknownAppMessage(appName))
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
		b.postEphemeralCommand(ctx, cmd, b.unknownAppMessage(appName))
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

func (b *Bot) handleApps(ctx context.Context, cmd slack.SlashCommand) {
	cfg := b.cfg.Load()
	if len(cfg.Apps) == 0 {
		b.postEphemeralCommand(ctx, cmd, "No apps configured.")
		return
	}

	var lines []string
	for _, app := range cfg.Apps {
		source := "operator"
		if app.SourceRepo != "" {
			source = app.SourceRepo
		}
		line := fmt.Sprintf("• *%s* (`%s`) — source: `%s`", app.App, app.Environment, source)
		if app.AutoDeploy {
			line += " — auto-deploy"
		}
		lines = append(lines, line)
	}

	b.postEphemeralCommand(ctx, cmd, "*Configured Apps:*\n"+strings.Join(lines, "\n"))
}

func (b *Bot) handleConflicts(ctx context.Context, cmd slack.SlashCommand) {
	h := b.cfg
	conflicts, err := config.LoadConflicts(h.Path(), h.DiscoveredPath())
	if err != nil {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Failed to check conflicts: %v", err))
		return
	}

	if len(conflicts) == 0 {
		b.postEphemeralCommand(ctx, cmd, "No config conflicts.")
		return
	}

	var lines []string
	for _, c := range conflicts {
		lines = append(lines, fmt.Sprintf(
			"• `%s` (`%s`) — repo `%s` blocked by operator config",
			c.App, c.Env, c.SourceRepo,
		))
	}
	b.postEphemeralCommand(ctx, cmd,
		"*Config Conflicts:*\nThe following repo-sourced apps are blocked by operator config. "+
			"Remove them from operator config for the repo definitions to take effect.\n"+
			strings.Join(lines, "\n"),
	)
}

func (b *Bot) handleSlashHelp(ctx context.Context, cmd slack.SlashCommand) {
	b.postEphemeralCommand(ctx, cmd, helpText(cmd.Command))
}

func (b *Bot) postEphemeralCommand(ctx context.Context, cmd slack.SlashCommand, text string) {
	_, err := b.slack.PostEphemeralContext(ctx, cmd.ChannelID, cmd.UserID, slack.MsgOptionText(text, false))
	if err != nil {
		b.log.Error("post ephemeral", zap.Error(err))
	}
}

// helpText returns the full help message. cmdName is the slash command name
// (e.g. "/deploy") used to make examples match the installation.
//
// App names in this bot include the environment (e.g. `myapp-dev`,
// `myapp-prod`). Use `apps` to see what's configured.
func helpText(cmdName string) string {
	return fmt.Sprintf(`*%s commands*
App names include the environment suffix (e.g. `+"`myapp-dev`"+`, `+"`myapp-prod`"+`). Use `+"`apps`"+` to list them.

• `+"`%s`"+`  — open the deploy modal
• `+"`%s <app-env>`"+`  — open the deploy modal pre-selected to an app
• `+"`%s list`"+`  — list pending deployments (alias: `+"`status`"+`)
• `+"`%s history [app-env]`"+`  — show recent completed deploys
• `+"`%s apps`"+`  — list all configured apps and their source
• `+"`%s conflicts`"+`  — list repo-sourced apps blocked by operator config
• `+"`%s tags <app-env>`"+`  — list recent ECR tags
• `+"`%s tags <app-env> <tag>`"+`  — check if a specific tag exists
• `+"`%s cancel <pr>`"+`  — cancel your own pending deployment
• `+"`%s nudge <pr>`"+`  — remind the approver
• `+"`%s rollback <app-env>`"+`  — deploy the previously merged tag
• `+"`%s help`"+`  — show this message`,
		cmdName,
		cmdName, cmdName, cmdName, cmdName, cmdName,
		cmdName, cmdName, cmdName, cmdName, cmdName, cmdName, cmdName)
}
