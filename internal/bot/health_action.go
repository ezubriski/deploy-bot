package bot

import (
	"context"
	"encoding/json"

	"github.com/slack-go/slack"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/healthcheck"
	"github.com/ezubriski/deploy-bot/internal/store"
)

// maybeStartHealthCheck launches a background health monitoring goroutine
// if the app has health_checks configured and providers are available.
func (b *Bot) maybeStartHealthCheck(ctx context.Context, cfg *config.Config, d *store.PendingDeploy, prNumber int) {
	if b.healthMonitor == nil {
		return
	}
	appCfg, ok := cfg.AppByName(d.App)
	if !ok || len(appCfg.HealthChecks) == 0 {
		return
	}

	checks := make([]healthcheck.Check, len(appCfg.HealthChecks))
	for i, hc := range appCfg.HealthChecks {
		checks[i] = healthcheck.Check{
			Provider:  hc.Provider,
			Name:      hc.EffectiveName(),
			Query:     hc.Query,
			Threshold: hc.Threshold,
		}
	}

	// Use the deploy's Slack message for threading. If unavailable
	// (e.g. ECR auto-deploy), the channel thread TS is empty and the
	// health check posts flat to the channel.
	slackThreadTS := d.SlackMessageTS
	slackChannel := d.SlackChannel
	if slackChannel == "" {
		slackChannel = cfg.Slack.DeployChannel
	}

	p := healthcheck.Params{
		App:           d.App,
		Environment:   d.Environment,
		Tag:           d.Tag,
		PRNumber:      prNumber,
		Checks:        checks,
		PollInterval:  cfg.HealthCheck.PollIntervalDuration(),
		PollDuration:  cfg.HealthCheck.PollDurationValue(),
		SlackChannel:  slackChannel,
		SlackThreadTS: slackThreadTS,
		RequesterID:   d.RequesterID,
	}

	go b.healthMonitor.Run(ctx, p)
}

// handleHealthRollbackClick processes the rollback button from a health
// check failure prompt. It opens the deploy modal in rollback mode with
// the failing app and its previous known-good tag pre-filled.
func (b *Bot) handleHealthRollbackClick(ctx context.Context, callback slack.InteractionCallback, action *slack.BlockAction) {
	var payload healthcheck.RollbackPayload
	if err := json.Unmarshal([]byte(action.Value), &payload); err != nil {
		b.log.Error("health rollback click: bad payload",
			zap.String("value", action.Value),
			zap.Error(err),
		)
		b.replyEphemeral(ctx, callback.Channel.ID, callback.User.ID,
			"Sorry, this rollback prompt is malformed — please use `/deploy rollback` instead.")
		return
	}

	b.log.Info("health rollback click",
		zap.String("app", payload.App),
		zap.String("env", payload.Environment),
		zap.String("user", callback.User.ID),
	)

	// Delegate to the existing /deploy rollback <app> flow.
	b.handleRollback(ctx, slack.SlashCommand{
		UserID:    callback.User.ID,
		ChannelID: callback.Channel.ID,
		TriggerID: callback.TriggerID,
	}, payload.App)
}
