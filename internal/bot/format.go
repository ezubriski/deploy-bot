package bot

import (
	"fmt"
	"strings"
	"time"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/store"
)

// formatHistory renders deployment history entries as a Slack message.
// Returns ("", nil) when entries is empty so callers can handle the
// no-history case with their own reply method.
func formatHistory(entries []store.HistoryEntry, appFilter string) string {
	if len(entries) == 0 {
		if appFilter != "" {
			return fmt.Sprintf("No deployment history for *%s*.", appFilter)
		}
		return "No deployment history."
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
	return header + "\n" + strings.Join(lines, "\n")
}

// formatApps renders the configured apps list as a Slack message.
func formatApps(cfg *config.Config) string {
	if len(cfg.Apps) == 0 {
		return "No apps configured."
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
	return "*Configured Apps:*\n" + strings.Join(lines, "\n")
}

// formatConflicts renders repo-sourced app conflicts as a Slack message.
func formatConflicts(conflicts []config.Conflict) string {
	if len(conflicts) == 0 {
		return "No config conflicts."
	}
	var lines []string
	for _, c := range conflicts {
		lines = append(lines, fmt.Sprintf(
			"• `%s` (`%s`) — repo `%s` blocked by operator config",
			c.App, c.Env, c.SourceRepo,
		))
	}
	return "*Config Conflicts:*\nThe following repo-sourced apps are blocked by operator config. " +
		"Remove them from operator config for the repo definitions to take effect.\n" +
		strings.Join(lines, "\n")
}

// formatTagList renders a tag list as a Slack message. Returns empty
// string when no tags are available so the caller can handle it.
func formatTagList(appName string, tags []string) string {
	if len(tags) == 0 {
		return fmt.Sprintf("No tags found for *%s* (cache may still be warming up).", appName)
	}
	lines := make([]string, len(tags))
	for i, t := range tags {
		lines[i] = fmt.Sprintf("• `%s`", t)
	}
	return fmt.Sprintf("*Recent tags for %s:*\n%s", appName, strings.Join(lines, "\n"))
}
