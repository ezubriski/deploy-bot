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
		b.openDeployModal(ctx, cmd, "", "", false)
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
	case parts[0] == "rollback":
		b.handleRollbackModal(ctx, cmd)
	case parts[0] == "apps":
		b.handleApps(ctx, cmd)
	case parts[0] == "conflicts":
		b.handleConflicts(ctx, cmd)
	case parts[0] == "help":
		b.handleSlashHelp(ctx, cmd)
	default:
		// Treat first arg as app name
		b.openDeployModal(ctx, cmd, parts[0], "", false)
	}
}

func (b *Bot) openDeployModal(ctx context.Context, cmd slack.SlashCommand, preSelectedApp, preSelectedTag string, isRollback bool) {
	// Validate deployer
	isMember, _, err := b.validator.IsMember(ctx, cmd.UserID)
	if err != nil {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Failed to validate permissions: %v", err))
		return
	}
	if !isMember {
		b.postEphemeralCommand(ctx, cmd, "You are not a member of the authorized team.")
		return
	}

	cfg := b.cfg.Load()
	staleDuration := cfg.StaleDuration()

	// Parse pre-selected app (FullName like "nginx-01-dev") into components.
	var selectedApp, selectedEnv string
	if preSelectedApp != "" {
		if appCfg, ok := cfg.AppByName(preSelectedApp); ok {
			selectedApp = appCfg.App
			selectedEnv = appCfg.Environment
		}
	}

	params := b.buildFilteredModalParams(ctx, cfg, selectedApp, selectedEnv, preSelectedTag, isRollback)
	params.StaleDuration = staleDuration.String()
	params.CommandName = cmd.Command

	modal := buildDeployModal(params)
	_, err = b.slack.OpenViewContext(ctx, cmd.TriggerID, modal)
	if err != nil {
		b.log.Error("open deploy modal", zap.Error(err))
	}
}

// buildFilteredModalParams builds DeployModalParams with bidirectional
// filtering between app name and environment. If selectedApp is set, only
// environments where that app exists are offered. If selectedEnv is set, only
// apps available in that environment are offered. Tags are only populated when
// both are selected.
func (b *Bot) buildFilteredModalParams(ctx context.Context, cfg *config.Config, selectedApp, selectedEnv, preSelectedTag string, isRollback bool) DeployModalParams {
	// Determine filtered option lists.
	var appNames, envNames []string
	if selectedEnv != "" {
		appNames = cfg.AppsForEnvironment(selectedEnv)
	} else {
		appNames = cfg.UniqueAppNames()
	}
	if selectedApp != "" {
		envNames = cfg.EnvironmentsForApp(selectedApp)
	} else {
		envNames = cfg.UniqueEnvironments()
	}

	appOptions := make([]*slack.OptionBlockObject, len(appNames))
	for i, name := range appNames {
		appOptions[i] = slack.NewOptionBlockObject(
			name,
			slack.NewTextBlockObject("plain_text", name, false, false),
			nil,
		)
	}
	envOptions := make([]*slack.OptionBlockObject, len(envNames))
	for i, env := range envNames {
		envOptions[i] = slack.NewOptionBlockObject(
			env,
			slack.NewTextBlockObject("plain_text", env, false, false),
			nil,
		)
	}

	params := DeployModalParams{
		AppNameOptions: appOptions,
		EnvOptions:     envOptions,
		SelectedApp:    selectedApp,
		SelectedEnv:    selectedEnv,
		SelectedTag:    preSelectedTag,
		IsRollback:     isRollback,
		HideManualTag:  isRollback,
	}

	// Only fetch tags when both app and env are selected.
	if selectedApp != "" && selectedEnv != "" {
		fullName := selectedApp + "-" + selectedEnv

		if isRollback {
			// Rollback: source tags from deployment history with deployed dates.
			entries, err := b.store.GetHistory(ctx, 100)
			if err == nil {
				cur, prev, ok := findRollbackEntries(entries, fullName)
				if ok {
					params.RollbackCurrent = cur.Tag
					params.RollbackCurrentDate = cur.CompletedAt
					params.RollbackTarget = prev.Tag
					params.RollbackTargetDate = prev.CompletedAt
					params.ExcludeTag = cur.Tag
					if preSelectedTag == "" {
						params.SelectedTag = prev.Tag
					}
				}
				// Build tag options from all unique approved deploys.
				seen := map[string]bool{}
				for _, e := range entries {
					if e.App == fullName && e.EventType == "approved" && !seen[e.Tag] {
						seen[e.Tag] = true
						label := fmt.Sprintf("%s (deployed %s)", e.Tag, e.CompletedAt.Format("Jan 2 15:04"))
						params.TagOptions = append(params.TagOptions, slack.NewOptionBlockObject(
							e.Tag,
							slack.NewTextBlockObject("plain_text", label, false, false),
							nil,
						))
					}
				}
			}
		} else {
			// Regular deploy: source tags from ECR cache with published dates.
			for _, t := range b.ecrCache.RecentTagsWithTime(fullName) {
				label := fmt.Sprintf("%s (published %s)", t.Tag, t.PushedAt.Format("Jan 2 15:04"))
				params.TagOptions = append(params.TagOptions, slack.NewOptionBlockObject(
					t.Tag,
					slack.NewTextBlockObject("plain_text", label, false, false),
					nil,
				))
			}
		}
	}

	return params
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
				all, err := b.store.GetAll(ctx)
				if err != nil {
					b.log.Warn("store: list deploys for suggestions", zap.Error(err))
				}
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
	go func() {
		defer wg.Done()
		b.warnIfErr("github: comment cancelled", b.gh.CommentCancelled(ctx, prNumber, requesterGH), zap.Int("pr", prNumber))
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
		if err := b.auditLog.Log(ctx, audit.AuditEvent{
			EventType:   audit.EventCancelled,
			Trigger:     audit.TriggerSlashCommand,
			App:         d.App,
			Environment: d.Environment,
			Tag:         d.Tag,
			PRNumber:    prNumber,
			PRURL:       d.PRURL,
			ActorEmail:  cancellerIdent.Email,
			ActorName:   cancellerIdent.Name,
		}); err != nil {
			b.log.Error("audit log", zap.Error(err))
		}
	}()
	go func() {
		defer wg.Done()
		if err := b.store.PushHistory(ctx, store.HistoryEntry{
			EventType:   audit.EventCancelled,
			App:         d.App,
			Environment: d.Environment,
			Tag:         d.Tag,
			PRNumber:    prNumber,
			PRURL:       d.PRURL,
			RequesterID: d.RequesterID,
			CompletedAt: time.Now(),
		}); err != nil {
			b.log.Warn("store: push history", zap.Error(err))
		}
	}()
	go func() {
		defer wg.Done()
		b.postSlack(ctx, cfg.Slack.DeployChannel, "notice",
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
	if d.ApproverID == "" {
		approver = "team"
	}
	_, _, err = b.slack.PostMessageContext(ctx, cfg.Slack.DeployChannel,
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

// findRollbackEntries scans entries (newest-first) for the two most recent
// approved deploys of appName. It returns (current, previous, true) when found,
// and (zero, zero, false) when fewer than two approved entries exist.
func findRollbackEntries(entries []store.HistoryEntry, appName string) (current, previous store.HistoryEntry, ok bool) {
	var approved []store.HistoryEntry
	for _, e := range entries {
		if e.App == appName && e.EventType == "approved" {
			approved = append(approved, e)
			if len(approved) == 2 {
				return approved[0], approved[1], true
			}
		}
	}
	return store.HistoryEntry{}, store.HistoryEntry{}, false
}

// findRollbackTag is a tag-only wrapper around findRollbackEntries.
func findRollbackTag(entries []store.HistoryEntry, appName string) (current, previous string, ok bool) {
	cur, prev, ok := findRollbackEntries(entries, appName)
	if !ok {
		return "", "", false
	}
	return cur.Tag, prev.Tag, true
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
	isMember, _, err := b.validator.IsMember(ctx, cmd.UserID)
	if err != nil {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Failed to validate permissions: %v", err))
		return
	}
	if !isMember {
		b.postEphemeralCommand(ctx, cmd, "You are not a member of the authorized team.")
		return
	}

	// If appName isn't a known FullName, try appending "-prod" since
	// rollback defaults to prod.
	cfg := b.cfg.Load()
	if _, found := cfg.AppByName(appName); !found {
		if _, found := cfg.AppByName(appName + "-prod"); found {
			appName = appName + "-prod"
		}
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
	b.openDeployModal(ctx, cmd, appName, rollbackTag, true)
}

// handleRollbackModal opens the deploy modal with environment defaulted to
// prod so the user can pick an app to roll back. Used when /deploy rollback
// is invoked without an app argument.
func (b *Bot) handleRollbackModal(ctx context.Context, cmd slack.SlashCommand) {
	isMember, _, err := b.validator.IsMember(ctx, cmd.UserID)
	if err != nil {
		b.postEphemeralCommand(ctx, cmd, fmt.Sprintf("Failed to validate permissions: %v", err))
		return
	}
	if !isMember {
		b.postEphemeralCommand(ctx, cmd, "You are not a member of the authorized team.")
		return
	}

	cfg := b.cfg.Load()
	params := b.buildFilteredModalParams(ctx, cfg, "", "prod", "", true)
	params.StaleDuration = cfg.StaleDuration().String()
	params.CommandName = cmd.Command

	modal := buildDeployModal(params)
	_, err = b.slack.OpenViewContext(ctx, cmd.TriggerID, modal)
	if err != nil {
		b.log.Error("open rollback modal", zap.Error(err))
	}
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
		line := fmt.Sprintf("• *%s* (`%s`) — source: `%s`", app.FullName(), app.Environment, source)
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
• `+"`%s rollback [app-env]`"+`  — deploy the previously merged tag (defaults to prod)
• `+"`%s help`"+`  — show this message`,
		cmdName,
		cmdName, cmdName, cmdName, cmdName, cmdName,
		cmdName, cmdName, cmdName, cmdName, cmdName, cmdName, cmdName)
}
