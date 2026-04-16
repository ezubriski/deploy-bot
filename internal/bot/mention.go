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
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/config"
	githubPkg "github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/store"
)

var mentionCommands = map[string]bool{
	"list":      true,
	"status":    true,
	"history":   true,
	"apps":      true,
	"conflicts": true,
	"tags":      true,
	"deploy":    true,
	"cancel":    true,
	"nudge":     true,
	"rollback":  true,
	"help":      true,
}

func (b *Bot) handleMention(ctx context.Context, evt queue.AppMentionEvent) {
	parts := strings.Fields(evt.Text)

	if len(parts) == 0 {
		b.replyMention(ctx, evt, b.mentionHelp())
		return
	}

	cmd := strings.ToLower(parts[0])
	if !mentionCommands[cmd] {
		b.replyMention(ctx, evt, fmt.Sprintf("Unknown command `%s`. %s", cmd, b.mentionHelp()))
		return
	}

	switch cmd {
	case "list", "status":
		envFilter := ""
		appFilter := ""
		if len(parts) >= 2 {
			envFilter = parts[1]
		}
		if len(parts) >= 3 {
			appFilter = parts[2]
		}
		b.handleMentionStatus(ctx, evt, envFilter, appFilter)
	case "history":
		appFilter := ""
		if len(parts) >= 2 {
			appFilter = parts[1]
		}
		b.handleMentionHistory(ctx, evt, appFilter)
	case "apps":
		b.handleMentionApps(ctx, evt)
	case "conflicts":
		b.handleMentionConflicts(ctx, evt)
	case "tags":
		if len(parts) >= 2 {
			b.handleMentionTags(ctx, evt, parts[1])
		} else {
			b.replyMentionError(ctx, evt, "Missing argument.", "Usage: `tags <app-env>`")
		}
	case "deploy":
		if len(parts) >= 3 {
			approver, reason := parseMentionDeployArgs(parts[3:])
			b.handleMentionDeploy(ctx, evt, parts[1], parts[2], approver, reason)
		} else {
			b.replyMentionError(ctx, evt, "Missing arguments.", "Usage: `deploy <app-env> <tag> [@approver] [reason...]`")
		}
	case "cancel":
		if len(parts) >= 2 {
			b.handleMentionCancel(ctx, evt, parts[1])
		} else {
			b.replyMentionError(ctx, evt, "Missing argument.", "Usage: `cancel <pr>`")
		}
	case "nudge":
		if len(parts) >= 2 {
			b.handleMentionNudge(ctx, evt, parts[1])
		} else {
			b.replyMentionError(ctx, evt, "Missing argument.", "Usage: `nudge <pr>`")
		}
	case "rollback":
		if len(parts) >= 2 {
			b.handleMentionRollback(ctx, evt, parts[1])
		} else {
			b.replyMentionError(ctx, evt, "Missing argument.", "Usage: `rollback <app-env>`")
		}
	case "help":
		b.replyMention(ctx, evt, b.mentionHelp())
	}
}

func (b *Bot) handleMentionStatus(ctx context.Context, evt queue.AppMentionEvent, envFilter, appFilter string) {
	deploys, err := b.store.GetAll(ctx)
	if err != nil {
		b.replyMention(ctx, evt, fmt.Sprintf("Failed to fetch deployments: %v", err))
		return
	}

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
		b.replyMention(ctx, evt, msg)
		return
	}

	now := time.Now()
	var lines []string
	for _, d := range deploys {
		age := now.Sub(d.RequestedAt).Round(time.Minute)
		lines = append(lines, fmt.Sprintf(
			"• *%s* (%s) `%s` — PR <%s|#%d> — %s — %s old — _%s_",
			d.App, d.Environment, d.Tag, d.PRURL, d.PRNumber, slackMention(d.RequesterID), age, d.State,
		))
	}
	b.replyMention(ctx, evt, "*Pending Deployments:*\n"+strings.Join(lines, "\n"))
}

func (b *Bot) handleMentionHistory(ctx context.Context, evt queue.AppMentionEvent, appFilter string) {
	const defaultLimit = 10

	entries, err := b.store.GetHistory(ctx, appFilter, 100)
	if err != nil {
		b.replyMention(ctx, evt, fmt.Sprintf("Failed to fetch history: %v", err))
		return
	}

	// Limit for channel display.
	now := time.Now()
	var lines []string
	count := 0
	for _, e := range entries {
		age := now.Sub(e.CompletedAt).Round(time.Minute)
		icon := eventIcon(e.EventType)
		lines = append(lines, fmt.Sprintf(
			"%s *%s* (%s) `%s` — <%s|#%d> — %s — %s ago",
			icon, e.App, e.Environment, e.Tag, e.PRURL, e.PRNumber, slackMention(e.RequesterID), age,
		))
		count++
		if count >= defaultLimit {
			break
		}
	}

	if len(lines) == 0 {
		msg := "No deployment history."
		if appFilter != "" {
			msg = fmt.Sprintf("No deployment history for *%s*.", appFilter)
		}
		b.replyMention(ctx, evt, msg)
		return
	}

	header := "*Recent Deployments:*"
	if appFilter != "" {
		header = fmt.Sprintf("*Deployments for %s:*", appFilter)
	}
	b.replyMention(ctx, evt, header+"\n"+strings.Join(lines, "\n"))
}

func (b *Bot) handleMentionApps(ctx context.Context, evt queue.AppMentionEvent) {
	cfg := b.cfg.Load()
	if len(cfg.Apps) == 0 {
		b.replyMention(ctx, evt, "No apps configured.")
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
	b.replyMention(ctx, evt, "*Configured Apps:*\n"+strings.Join(lines, "\n"))
}

func (b *Bot) handleMentionConflicts(ctx context.Context, evt queue.AppMentionEvent) {
	h := b.cfg
	conflicts, err := config.LoadConflicts(h.Path(), h.DiscoveredPath())
	if err != nil {
		b.replyMention(ctx, evt, fmt.Sprintf("Failed to check conflicts: %v", err))
		return
	}
	if len(conflicts) == 0 {
		b.replyMention(ctx, evt, "No config conflicts.")
		return
	}
	var lines []string
	for _, c := range conflicts {
		lines = append(lines, fmt.Sprintf(
			"• `%s` (`%s`) — repo `%s` blocked by operator config",
			c.App, c.Env, c.SourceRepo,
		))
	}
	b.replyMention(ctx, evt,
		"*Config Conflicts:*\nThe following repo-sourced apps are blocked by operator config. "+
			"Remove them from operator config for the repo definitions to take effect.\n"+
			strings.Join(lines, "\n"),
	)
}

func (b *Bot) handleMentionTags(ctx context.Context, evt queue.AppMentionEvent, appName string) {
	if _, ok := b.cfg.Load().AppByName(appName); !ok {
		b.replyMention(ctx, evt, b.unknownAppMessage(appName))
		return
	}
	tags := b.ecrCache.Tags(appName, 10)
	if len(tags) == 0 {
		b.replyMention(ctx, evt, fmt.Sprintf("No tags found for *%s* (cache may still be warming up).", appName))
		return
	}
	lines := make([]string, len(tags))
	for i, t := range tags {
		lines[i] = fmt.Sprintf("• `%s`", t)
	}
	b.replyMention(ctx, evt,
		fmt.Sprintf("*Recent tags for %s:*\n%s", appName, strings.Join(lines, "\n")),
	)
}

func (b *Bot) handleMentionCancel(ctx context.Context, evt queue.AppMentionEvent, prArg string) {
	prNumber, err := strconv.Atoi(prArg)
	if err != nil {
		b.replyMention(ctx, evt, "Invalid PR number.")
		return
	}

	cfg := b.cfg.Load()
	d, err := b.store.Get(ctx, cfg.GitHub.Org, cfg.GitHub.Repo, prNumber)
	if err != nil || d == nil {
		b.replyMention(ctx, evt, fmt.Sprintf("Deployment #%d not found.", prNumber))
		return
	}

	if d.RequesterID != evt.UserID {
		b.replyMention(ctx, evt, "You can only cancel your own deployments.")
		return
	}

	cancellerIdent, err := b.validator.ResolveIdentity(ctx, evt.UserID)
	requesterGH := cancellerIdent.GitHubLogin
	if err != nil || requesterGH == "" {
		requesterGH = "slack:" + evt.UserID
	}

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
		b.warnIfErr("github: remove pending label", b.gh.RemoveLabel(ctx, prNumber, b.cfg.Load().PendingLabel()), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		b.errIfErr("store: release lock", b.store.ReleaseLock(ctx, d.Environment, d.App), zap.String("env", d.Environment), zap.String("app", d.App))
	}()
	go func() {
		defer wg.Done()
		b.errIfErr("store: delete pending", b.store.Delete(ctx, d.GitHubOrg, d.GitHubRepo, prNumber), zap.Int("pr", prNumber))
	}()
	go func() {
		defer wg.Done()
		if err := b.auditLog.Log(ctx, audit.AuditEvent{
			EventType:   audit.EventCancelled,
			Trigger:     audit.TriggerMention,
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
		if err := b.store.PushHistory(ctx, store.HistoryFromPending(d, audit.EventCancelled)); err != nil {
			b.log.Warn("store: push history", zap.Error(err))
		}
	}()
	go func() {
		defer wg.Done()
		b.replyMention(ctx, evt, fmt.Sprintf(
			"Deployment of *%s* (%s) `%s` (<%s|PR #%d>) *cancelled* by <@%s>.",
			d.App, d.Environment, d.Tag, d.PRURL, prNumber, evt.UserID,
		))
	}()
	b.metrics.RecordDeploy(d.App, audit.EventCancelled)
	wg.Wait()
	b.updatePendingGauge(ctx)
	b.log.Info("deployment cancelled via mention", zap.Int("pr", prNumber), zap.String("user", cancellerIdent.String()))
}

func (b *Bot) handleMentionNudge(ctx context.Context, evt queue.AppMentionEvent, prArg string) {
	prNumber, err := strconv.Atoi(prArg)
	if err != nil {
		b.replyMention(ctx, evt, "Invalid PR number.")
		return
	}

	cfg := b.cfg.Load()
	d, err := b.store.Get(ctx, cfg.GitHub.Org, cfg.GitHub.Repo, prNumber)
	if err != nil || d == nil {
		b.replyMention(ctx, evt, fmt.Sprintf("Deployment #%d not found.", prNumber))
		return
	}

	remaining := time.Until(d.ExpiresAt).Round(time.Minute)
	b.replyMention(ctx, evt, fmt.Sprintf(
		"%s — reminder: deployment of *%s* (%s) `%s` by %s is waiting for your approval. Expires in *%s*. <%s|PR #%d>",
		slackMention(d.ApproverID), d.App, d.Environment, d.Tag, slackMention(d.RequesterID), remaining, d.PRURL, d.PRNumber,
	))
}

// parseMentionDeployArgs extracts an optional @approver mention and the
// remaining reason text from the args after app and tag.
// Input parts are the tokens after `deploy <app> <tag>`.
func parseMentionDeployArgs(parts []string) (approverID, reason string) {
	var reasonParts []string
	for _, p := range parts {
		if uid := extractUserID(p); uid != "" && approverID == "" {
			approverID = uid
		} else {
			reasonParts = append(reasonParts, p)
		}
	}
	return approverID, strings.Join(reasonParts, " ")
}

// extractUserID pulls a Slack user ID from a mention token like "<@U12345>"
// or "<@U12345|name>". Returns empty string if not a mention.
func extractUserID(token string) string {
	if !strings.HasPrefix(token, "<@") || !strings.HasSuffix(token, ">") {
		return ""
	}
	inner := token[2 : len(token)-1]
	// Handle "<@U12345|displayname>" format.
	if idx := strings.Index(inner, "|"); idx >= 0 {
		inner = inner[:idx]
	}
	return inner
}

func (b *Bot) handleMentionDeploy(ctx context.Context, evt queue.AppMentionEvent, appName, tag, approverID, reason string) {
	const usage = "Usage: `deploy <app-env> <tag> [@approver] [reason...]`"

	isMember, _, err := b.validator.IsMember(ctx, evt.UserID)
	if err != nil {
		b.replyMentionError(ctx, evt, fmt.Sprintf("Failed to validate permissions: %v", err), usage)
		return
	}
	if !isMember {
		b.replyMentionError(ctx, evt, "You are not a member of the authorized team.", usage)
		return
	}

	cfg := b.cfg.Load()
	appCfg, ok := cfg.AppByName(appName)
	if !ok {
		b.replyMentionError(ctx, evt, b.unknownAppMessage(appName), usage)
		return
	}
	env := appCfg.Environment

	// Validate approver if specified.
	if approverID != "" {
		isApprover, _, err := b.validator.IsMember(ctx, approverID)
		if err != nil {
			b.replyMentionError(ctx, evt, fmt.Sprintf("Could not validate approver: %v", err), usage)
			return
		}
		if !isApprover {
			b.replyMentionError(ctx, evt, fmt.Sprintf("<@%s> is not a member of the authorized team.", approverID), usage)
			return
		}
	}

	valid, err := b.ecrCache.ValidateTag(ctx, appName, tag)
	if err != nil || !valid {
		b.replyMentionError(ctx, evt, fmt.Sprintf("Tag `%s` not found in ECR for *%s*.", tag, appName), usage)
		return
	}

	lockTTL := cfg.LockTTL()
	acquired, err := b.store.AcquireLock(ctx, env, appName, evt.UserID, lockTTL)
	if err != nil {
		b.replyMentionError(ctx, evt, "Could not check deploy lock. Please try again.", "")
		return
	}
	if !acquired {
		msg := fmt.Sprintf("A deployment of *%s* (%s) is already in progress.", appName, env)
		if existing, err := b.store.GetByEnvApp(ctx, env, appName); err != nil {
			b.log.Warn("store: lookup existing deploy", zap.String("env", env), zap.String("app", appName), zap.Error(err))
		} else if existing != nil {
			msg = fmt.Sprintf("A deployment of *%s* (%s) is already in progress (<%s|PR #%d>).", appName, env, existing.PRURL, existing.PRNumber)
		}
		b.replyMention(ctx, evt, msg)
		return
	}

	baseBranch, err := b.gh.GetDefaultBranch(ctx)
	if err != nil {
		b.log.Error("mention deploy: get default branch", zap.Error(err))
		b.errIfErr("store: release lock", b.store.ReleaseLock(ctx, env, appName), zap.String("env", env), zap.String("app", appName))
		b.replyMentionError(ctx, evt, "Failed to get default branch from GitHub.", "")
		return
	}

	requesterIdent, err := b.validator.ResolveIdentity(ctx, evt.UserID)
	requesterGH := requesterIdent.GitHubLogin
	if err != nil || requesterGH == "" {
		requesterGH = "slack:" + evt.UserID
	}

	prNumber, prURL, err := b.gh.CreateDeployPR(ctx, githubPkg.CreatePRParams{
		App:              appName,
		Environment:      env,
		Tag:              tag,
		KustomizePath:    appCfg.KustomizePath,
		BaseBranch:       baseBranch,
		Requester:        requesterGH,
		Reason:           reason,
		RequesterSlackID: evt.UserID,
	})
	if err != nil {
		b.errIfErr("store: release lock", b.store.ReleaseLock(ctx, env, appName), zap.String("env", env), zap.String("app", appName))
		if errors.Is(err, githubPkg.ErrNoChange) {
			b.replyMention(ctx, evt, fmt.Sprintf("`%s` (`%s`) is already running `%s` — no changes to deploy.", appName, env, tag))
			return
		}
		b.replyMentionError(ctx, evt, fmt.Sprintf("Failed to create PR: %v", err), "")
		return
	}

	staleDuration := cfg.StaleDuration()
	expiresAt := time.Now().Add(staleDuration)

	d := &store.PendingDeploy{
		GitHubOrg:   cfg.GitHub.Org,
		GitHubRepo:  cfg.GitHub.Repo,
		App:         appName,
		Environment: env,
		Tag:         tag,
		PRNumber:    prNumber,
		PRURL:       prURL,
		Requester:   requesterGH,
		RequesterID: evt.UserID,
		ApproverID:  approverID,
		Reason:      reason,
		RequestedAt: time.Now(),
		ExpiresAt:   expiresAt,
		State:       store.StatePending,
	}
	if err := b.store.Set(ctx, d, staleDuration); err != nil {
		b.log.Error("mention deploy: store deploy", zap.Error(err))
	}

	b.warnIfErr("github: add labels", b.gh.AddLabels(ctx, prNumber, []string{cfg.DeployLabel(), cfg.PendingLabel()}), zap.Int("pr", prNumber))
	b.warnIfErr("github: comment requested", b.gh.CommentRequested(ctx, prNumber, requesterGH, appName, tag, reason), zap.Int("pr", prNumber))

	deployChannel := cfg.Slack.DeployChannel
	slackChannel, slackTS := b.postSlackWithHandle(ctx, deployChannel, "notice",
		buildApproverMessage(pendingInfo{
			App:         appName,
			Environment: env,
			Tag:         tag,
			PRNumber:    prNumber,
			PRURL:       prURL,
			RequesterID: evt.UserID,
			ApproverID:  approverID,
			Reason:      reason,
		})...,
	)
	if slackTS != "" {
		if err := b.store.SetSlackHandle(ctx, cfg.GitHub.Org, cfg.GitHub.Repo, prNumber, slackChannel, slackTS); err != nil {
			b.log.Warn("mention deploy: update deploy with slack handle", zap.Error(err))
		}
	}

	if err := b.auditLog.Log(ctx, audit.AuditEvent{
		EventType:   audit.EventRequested,
		Trigger:     audit.TriggerMention,
		App:         appName,
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

	b.metrics.RecordDeploy(appName, audit.EventRequested)
	b.updatePendingGauge(ctx)

	b.replyMention(ctx, evt, fmt.Sprintf(
		"Deployment of *%s* (%s) `%s` requested — <%s|PR #%d> created. Awaiting approval in <#%s>.\n_Tip: the slash command (`/deploy`) provides a guided experience with tag selection and approver assignment._",
		appName, env, tag, prURL, prNumber, deployChannel,
	))
	b.log.Info("deployment requested via mention", zap.String("app", appName), zap.String("tag", tag), zap.Int("pr", prNumber), zap.String("requester", requesterIdent.String()))
}

func (b *Bot) handleMentionRollback(ctx context.Context, evt queue.AppMentionEvent, appName string) {
	isMember, _, err := b.validator.IsMember(ctx, evt.UserID)
	if err != nil {
		b.replyMentionError(ctx, evt, fmt.Sprintf("Failed to validate permissions: %v", err), "Usage: `rollback <app-env>`")
		return
	}
	if !isMember {
		b.replyMentionError(ctx, evt, "You are not a member of the authorized team.", "Usage: `rollback <app-env>`")
		return
	}

	entries, err := b.store.GetHistory(ctx, appName, 100)
	if err != nil {
		b.replyMentionError(ctx, evt, fmt.Sprintf("Failed to fetch history: %v", err), "")
		return
	}

	_, rollbackTag, ok := findRollbackTag(entries, appName)
	if !ok {
		b.replyMentionError(ctx, evt,
			fmt.Sprintf("Not enough deployment history for *%s* to determine a rollback target.", appName),
			"Usage: `rollback <app-env>`")
		return
	}

	// Initiate the deploy with the rollback tag.
	b.handleMentionDeploy(ctx, evt, appName, rollbackTag, "", fmt.Sprintf("rollback to %s", rollbackTag))
}

func (b *Bot) replyMention(ctx context.Context, evt queue.AppMentionEvent, text string) {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if evt.ThreadTS != "" {
		opts = append(opts, slack.MsgOptionTS(evt.ThreadTS))
	}
	_, _, err := b.slack.PostMessageContext(ctx, evt.Channel, opts...)
	if err != nil {
		b.log.Error("reply to mention", zap.Error(err))
	}
}

// replyMentionError replies with the error, usage hint, and a nudge toward
// the slash command for a more guided experience.
func (b *Bot) replyMentionError(ctx context.Context, evt queue.AppMentionEvent, errMsg, usage string) {
	text := errMsg
	if usage != "" {
		text += "\n" + usage
	}
	text += "\n_Try the slash command (`/deploy`) for a guided experience with dropdowns and validation._"
	b.replyMention(ctx, evt, text)
}

func (b *Bot) mentionHelp() string {
	return `*Available commands*
App names include the environment suffix (e.g. ` + "`myapp-dev`" + `, ` + "`myapp-prod`" + `). Use ` + "`apps`" + ` to list them.

• ` + "`deploy <app-env> <tag> [@approver] [reason]`" + `  — create a deploy PR
• ` + "`list`" + `  — list pending deployments (alias: ` + "`status`" + `)
• ` + "`history [app-env]`" + `  — show recent completed deploys
• ` + "`apps`" + `  — list all configured apps and their source
• ` + "`conflicts`" + `  — list repo-sourced apps blocked by operator config
• ` + "`tags <app-env>`" + `  — list recent ECR tags
• ` + "`cancel <pr>`" + `  — cancel your own pending deployment
• ` + "`nudge <pr>`" + `  — remind the approver
• ` + "`rollback <app-env>`" + `  — deploy the previously merged tag
• ` + "`help`" + `  — show this message
_The slash command provides a guided modal with dropdowns and validation._`
}

// unknownAppMessage builds a helpful error message when an app name doesn't
// match any configured entry, listing available apps so users don't have to guess.
// App names in this bot include the environment (e.g. myapp-dev, myapp-prod).
func (b *Bot) unknownAppMessage(name string) string {
	cfg := b.cfg.Load()
	if len(cfg.Apps) == 0 {
		return fmt.Sprintf("Unknown app `%s` — no apps are configured.", name)
	}
	var names []string
	for _, a := range cfg.Apps {
		names = append(names, fmt.Sprintf("`%s`", a.FullName()))
	}
	return fmt.Sprintf("Unknown app `%s`. App names include the environment — available apps:\n%s", name, strings.Join(names, ", "))
}
