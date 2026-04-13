package healthcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/slackclient"
)

// ActionHealthRollback is the block action ID for the rollback button
// posted when a health check fails at the end of the monitoring window.
const ActionHealthRollback = "action_health_rollback"

// QueryResult is a provider-agnostic query result. Each provider adapter
// converts its native response into this form.
type QueryResult struct {
	// Value is the first numeric value extracted from the query response.
	Value float64
	// OK is true if a numeric value was successfully extracted.
	OK bool
}

// MetricsQuerier is the interface that each metrics provider implements.
// The query string is provider-specific (DQL for Dynatrace, NRQL for
// New Relic, etc.).
type MetricsQuerier interface {
	Query(ctx context.Context, query string) (*QueryResult, error)
}

// Monitor runs post-deploy health check polling against one or more
// metrics providers and reports results in the deploy Slack thread.
type Monitor struct {
	providers map[string]MetricsQuerier
	slack     slackclient.Poster
	log       *zap.Logger
}

// NewMonitor creates a health check monitor. The providers map is keyed
// by provider name (e.g. "dynatrace") matching config.ProviderDynatrace.
func NewMonitor(providers map[string]MetricsQuerier, slack slackclient.Poster, log *zap.Logger) *Monitor {
	return &Monitor{providers: providers, slack: slack, log: log}
}

// Check defines a single health check to evaluate during monitoring.
type Check struct {
	Provider  string
	Name      string
	Query     string
	Threshold string
}

// Params configures a single health monitoring run.
type Params struct {
	App           string
	Environment   string
	Tag           string
	PRNumber      int
	Checks        []Check
	PollInterval  time.Duration
	PollDuration  time.Duration
	SlackChannel  string
	SlackThreadTS string
	RequesterID   string
}

// RollbackPayload is the JSON value attached to the rollback action button.
type RollbackPayload struct {
	App         string `json:"app"`
	Environment string `json:"environment"`
	Tag         string `json:"tag"`
	PRNumber    int    `json:"pr_number"`
}

// Run executes the health check polling loop. It posts a single status
// message in the deploy thread and updates it on each poll interval.
// All checks must pass (AND logic). If any check is unhealthy at the
// end of the monitoring window, a rollback button is posted.
// This method blocks for up to PollDuration.
func (m *Monitor) Run(ctx context.Context, p Params) {
	m.log.Info("health check: starting monitoring",
		zap.String("app", p.App),
		zap.String("env", p.Environment),
		zap.String("tag", p.Tag),
		zap.Int("checks", len(p.Checks)),
		zap.Duration("poll_interval", p.PollInterval),
		zap.Duration("poll_duration", p.PollDuration),
	)

	threadOpts := []slack.MsgOption{slack.MsgOptionTS(p.SlackThreadTS)}

	// Post the initial status message.
	checkNames := make([]string, len(p.Checks))
	for i, c := range p.Checks {
		checkNames[i] = c.Name
	}
	statusText := fmt.Sprintf(":stethoscope: Health check started for *%s* (%s) `%s` — monitoring for %s (%s).",
		p.App, p.Environment, p.Tag, p.PollDuration, strings.Join(checkNames, ", "))
	_, statusTS, err := m.slack.PostMessageContext(ctx, p.SlackChannel,
		append([]slack.MsgOption{slack.MsgOptionText(statusText, false)}, threadOpts...)...,
	)
	if err != nil {
		m.log.Error("health check: post initial status", zap.Error(err))
		return
	}

	deadline := time.After(p.PollDuration)
	ticker := time.NewTicker(p.PollInterval)
	defer ticker.Stop()

	var lastAllHealthy bool
	var lastSummaries []string
	checkCount := 0

	for {
		select {
		case <-ctx.Done():
			m.log.Info("health check: context cancelled", zap.String("app", p.App))
			return
		case <-deadline:
			// Final evaluation — all checks must have passed on the last tick.
			if lastAllHealthy {
				m.updateStatus(ctx, p.SlackChannel, statusTS,
					fmt.Sprintf(":white_check_mark: Health check *passed* for *%s* (%s) `%s` after %d rounds.\n%s",
						p.App, p.Environment, p.Tag, checkCount, formatSummaries(lastSummaries)),
				)
			} else {
				m.updateStatus(ctx, p.SlackChannel, statusTS,
					fmt.Sprintf(":x: Health check *failed* for *%s* (%s) `%s` after %d rounds.\n%s",
						p.App, p.Environment, p.Tag, checkCount, formatSummaries(lastSummaries)),
				)
				m.postRollbackPrompt(ctx, p)
			}
			return
		case <-ticker.C:
			checkCount++
			allHealthy, summaries := m.runChecks(ctx, p)
			lastAllHealthy = allHealthy
			lastSummaries = summaries

			status := ":large_green_circle:"
			if !allHealthy {
				status = ":red_circle:"
			}
			m.updateStatus(ctx, p.SlackChannel, statusTS,
				fmt.Sprintf("%s Health check for *%s* (%s) `%s` — round %d\n%s",
					status, p.App, p.Environment, p.Tag, checkCount, formatSummaries(summaries)),
			)
		}
	}
}

// runChecks evaluates all checks. Returns true only if every check is healthy.
func (m *Monitor) runChecks(ctx context.Context, p Params) (allHealthy bool, summaries []string) {
	allHealthy = true
	for _, c := range p.Checks {
		healthy, summary := m.runSingleCheck(ctx, p.App, c)
		summaries = append(summaries, fmt.Sprintf("> *%s*: %s", c.Name, summary))
		if !healthy {
			allHealthy = false
		}
	}
	return allHealthy, summaries
}

func (m *Monitor) runSingleCheck(ctx context.Context, app string, c Check) (healthy bool, summary string) {
	querier, ok := m.providers[c.Provider]
	if !ok {
		m.log.Error("health check: no querier for provider",
			zap.String("app", app),
			zap.String("provider", c.Provider),
		)
		return false, fmt.Sprintf("provider %q not configured", c.Provider)
	}

	result, err := querier.Query(ctx, c.Query)
	if err != nil {
		m.log.Warn("health check: query failed",
			zap.String("app", app),
			zap.String("check", c.Name),
			zap.Error(err),
		)
		return false, fmt.Sprintf("query error: %v", err)
	}

	if !result.OK {
		return false, "no numeric value returned by query"
	}

	healthy, summary, err = EvaluateThreshold(result.Value, c.Threshold)
	if err != nil {
		m.log.Warn("health check: threshold evaluation failed",
			zap.String("app", app),
			zap.String("check", c.Name),
			zap.Error(err),
		)
		return false, fmt.Sprintf("threshold error: %v", err)
	}
	return healthy, summary
}

func formatSummaries(summaries []string) string {
	return strings.Join(summaries, "\n")
}

func (m *Monitor) updateStatus(ctx context.Context, channel, ts, text string) {
	_, _, _, err := m.slack.UpdateMessageContext(ctx, channel, ts,
		slack.MsgOptionText(text, false),
	)
	if err != nil {
		m.log.Warn("health check: update status message", zap.Error(err))
	}
}

func (m *Monitor) postRollbackPrompt(ctx context.Context, p Params) {
	payload, err := json.Marshal(RollbackPayload{
		App:         p.App,
		Environment: p.Environment,
		Tag:         p.Tag,
		PRNumber:    p.PRNumber,
	})
	if err != nil {
		m.log.Error("health check: marshal rollback payload", zap.Error(err))
		return
	}

	text := fmt.Sprintf(
		":warning: *%s* (%s) `%s` is unhealthy after the monitoring window. Consider rolling back.",
		p.App, p.Environment, p.Tag,
	)

	btnRollback := slack.NewButtonBlockElement(
		ActionHealthRollback, string(payload),
		slack.NewTextBlockObject("plain_text", "Roll back", false, false),
	)
	btnRollback.Style = "danger"

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
		slack.NewActionBlock("", btnRollback),
	}

	threadOpts := []slack.MsgOption{slack.MsgOptionTS(p.SlackThreadTS)}
	opts := append([]slack.MsgOption{slack.MsgOptionBlocks(blocks...)}, threadOpts...)

	if _, _, err := m.slack.PostMessageContext(ctx, p.SlackChannel, opts...); err != nil {
		m.log.Error("health check: post rollback prompt", zap.Error(err))
	}
}
