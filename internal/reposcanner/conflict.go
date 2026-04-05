package reposcanner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	slackPkg "github.com/slack-go/slack"

	"github.com/ezubriski/deploy-bot/internal/slackclient"
)

const defaultWarnCooldown = 20 * time.Minute

// conflictInfo describes a collision between operator config and a
// repo-sourced entry, or between two repo-sourced entries.
type conflictInfo struct {
	App        string
	Env        string
	SourceRepo string
	Reason     string // "app+environment" or "kustomize_path"
	Detail     string // e.g. the conflicting path or the other app name
}

// conflictTracker batches and rate-limits Slack warnings. Conflicts are posted
// as a single batched message at most once per cooldown period. If the set of
// conflicts hasn't changed since the last warning, no message is sent.
type conflictTracker struct {
	mu       sync.Mutex
	warned   map[string]bool // key = "app\x00env" — conflicts included in last warning
	lastWarn time.Time
	cooldown time.Duration
	// nowFunc is injectable for testing; defaults to time.Now.
	nowFunc func() time.Time
}

func newConflictTracker() *conflictTracker {
	return &conflictTracker{
		warned:   make(map[string]bool),
		cooldown: defaultWarnCooldown,
		nowFunc:  time.Now,
	}
}

// emitWarnings posts a single batched Slack message for conflicts, rate-limited
// to at most once per cooldown period. Resolved conflicts reset their tracked
// state so they trigger a new warning if reintroduced.
func (ct *conflictTracker) emitWarnings(ctx context.Context, slack slackclient.Poster, channel, configFile string, conflicts map[string]conflictInfo) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if channel == "" {
		return
	}

	// Clear tracked state for resolved conflicts.
	for key := range ct.warned {
		if _, still := conflicts[key]; !still {
			delete(ct.warned, key)
		}
	}

	// Collect conflicts that haven't been warned about yet.
	var newConflicts []conflictInfo
	for key, info := range conflicts {
		if !ct.warned[key] {
			newConflicts = append(newConflicts, info)
		}
	}

	if len(newConflicts) == 0 {
		return
	}

	// Rate-limit: don't post more often than cooldown.
	now := ct.nowFunc()
	if !ct.lastWarn.IsZero() && now.Sub(ct.lastWarn) < ct.cooldown {
		return
	}

	// Build a single batched message.
	repoLink := func(repo string) string {
		return fmt.Sprintf("<https://github.com/%s/blob/main/%s|%s>", repo, configFile, repo)
	}

	var lines []string
	for _, info := range newConflicts {
		switch info.Reason {
		case "kustomize_path":
			lines = append(lines, fmt.Sprintf(
				"- `%s` (`%s`) from %s — `kustomize_path` conflicts with %s. "+
					"Each app must target a unique kustomization file. Update the `kustomize_path` in one of the configs to resolve this.",
				info.App, info.Env, repoLink(info.SourceRepo), info.Detail))
		default:
			lines = append(lines, fmt.Sprintf(
				"- `%s` (`%s`) from %s — already defined in operator config. "+
					"Remove the app from operator config to use the repo-sourced definition, or remove it from `%s` to keep the operator definition.",
				info.App, info.Env, repoLink(info.SourceRepo), configFile))
		}
	}
	msg := fmt.Sprintf(
		"*Config conflicts detected* — the following repo-sourced apps have been excluded:\n%s",
		strings.Join(lines, "\n"),
	)
	_, _, _ = slack.PostMessageContext(ctx, channel, slackPkg.MsgOptionText(msg, false))

	// Mark all as warned.
	for key := range conflicts {
		ct.warned[key] = true
	}
	ct.lastWarn = now
}

// marshalDiscoveredApps serialises the discovered apps to JSON with
// indentation for readability in the ConfigMap.
func marshalDiscoveredApps(da interface{}) ([]byte, error) {
	return json.MarshalIndent(da, "", "  ")
}
