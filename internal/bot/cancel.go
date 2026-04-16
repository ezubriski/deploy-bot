package bot

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/store"
)

// cancelParams captures the entry-point-specific differences between
// slash-command and @mention cancel flows.
type cancelParams struct {
	userID       string           // Slack user ID of the canceller
	userName     string           // fallback display name (empty → "slack:<userID>")
	trigger      string           // audit trigger (audit.TriggerSlashCommand or TriggerMention)
	replyError   func(msg string) // how to respond with errors (ephemeral for slash, channel for mention)
	replySuccess func(msg string) // how to post the cancellation notice
	allowECR     bool             // true if empty RequesterID should be cancellable (ECR-originated deploys)
}

// doCancelDeploy is the shared cancel implementation used by both
// handleCancel (slash command) and handleMentionCancel (@mention).
func (b *Bot) doCancelDeploy(ctx context.Context, prArg string, p cancelParams) {
	prNumber, err := strconv.Atoi(prArg)
	if err != nil {
		p.replyError("Invalid PR number.")
		return
	}

	cfg := b.cfg.Load()
	d, err := b.store.Get(ctx, cfg.GitHub.Org, cfg.GitHub.Repo, prNumber)
	if err != nil || d == nil {
		p.replyError(fmt.Sprintf("Deployment #%d not found.", prNumber))
		return
	}

	if d.RequesterID != p.userID {
		if !p.allowECR || d.RequesterID != "" {
			p.replyError("You can only cancel your own deployments.")
			return
		}
	}

	cancellerIdent, err := b.validator.ResolveIdentity(ctx, p.userID)
	requesterGH := cancellerIdent.GitHubLogin
	if err != nil || requesterGH == "" {
		requesterGH = p.userName
		if requesterGH == "" {
			requesterGH = "slack:" + p.userID
		}
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
		b.warnIfErr("github: remove pending label", b.gh.RemoveLabel(ctx, prNumber, cfg.PendingLabel()), zap.Int("pr", prNumber))
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
			Trigger:     p.trigger,
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
		p.replySuccess(fmt.Sprintf(
			"Deployment of *%s* (%s) `%s` (<%s|PR #%d>) *cancelled* by <@%s>.",
			d.App, d.Environment, d.Tag, d.PRURL, prNumber, p.userID,
		))
	}()
	b.metrics.RecordDeploy(d.App, audit.EventCancelled)
	wg.Wait()
	b.updatePendingGauge(ctx)
	b.log.Info("deployment cancelled", zap.Int("pr", prNumber), zap.String("user", cancellerIdent.String()))
}
