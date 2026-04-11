package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/sanitize"
	"github.com/ezubriski/deploy-bot/internal/store"
)

// lateArrivalThreshold is the age at which an ArgoCD failure notification
// stops being framed as "your deploy just broke" and starts being framed
// as "investigate before rolling back". Set to 2h: short enough that a
// normal deploy → sync cycle (minutes) always gets the alarming framing,
// long enough that runtime issues on stable deploys are clearly distinct.
const lateArrivalThreshold = 2 * time.Hour

// maxFailingResources caps the number of non-healthy resources we render
// inline in the alarming Slack message. Sized so a worst-case many-pod
// CrashLoopBackOff doesn't blow past Slack's 3000-char section text
// limit; any overflow renders as "…and N more".
const maxFailingResources = 10

// argocdResource mirrors the shape the reference webhook template emits
// for each entry in .app.status.resources. Fields with empty values are
// filtered during rendering, so optional fields (healthStatus,
// healthMessage) are tolerated.
type argocdResource struct {
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	Namespace     string `json:"namespace,omitempty"`
	SyncStatus    string `json:"syncStatus,omitempty"`
	HealthStatus  string `json:"healthStatus,omitempty"`
	HealthMessage string `json:"healthMessage,omitempty"`
}

// handleArgoCDNotification is the worker-side dispatch for an ArgoCD
// lifecycle notification that the receiver enqueued onto argocd:events.
//
// Phase 3 implementation: correlates the event to the originating deploy
// by gitops SHA, then posts per-trigger:
//
//   - on-sync-succeeded: a quiet threaded reply under the original deploy
//     message. If no original-message handle is stored, the reply is
//     dropped entirely — sync-succeeded is the quiet path and doesn't
//     justify a standalone channel post.
//
//   - on-sync-failed / on-health-degraded: an unmissable top-level
//     message in the deploy channel with siren emojis, a requester ping,
//     per-resource failure detail from the payload, and a permalink back
//     to the original deploy. Late-arriving notifications (deploy > 2h
//     old) are re-framed with a calmer tone and a "may not be related,
//     investigate first" note.
//
// Unmatched SHAs (no history entry, deploy made via another tool, or
// entry aged out of the 100-entry history window) are logged and
// dropped. The operator doc calls this out as expected behavior — the
// whole value proposition of this handler is "the deploy-bot knows who
// deployed what," and without a history entry we have neither.
//
// Phase 4 will add the separate top-level rollback prompt with [Roll
// back] and [Dismiss] buttons.
func (b *Bot) handleArgoCDNotification(ctx context.Context, evt queue.ArgoCDNotificationEvent) {
	entry, err := b.store.FindHistoryBySHA(ctx, evt.GitopsCommitSHA)
	if err != nil {
		// Redis lookup failed — treat as unmatched rather than retrying.
		// The dedupe cache has already absorbed this notification, so a
		// retry from ArgoCD would just be suppressed anyway. Recorded as
		// lookup_error so operators can alert on a non-zero rate: a
		// transient blip is expected, a sustained rate means correlation
		// is broken and every incident alert is being silently eaten.
		b.metrics.RecordArgoCDNotification(evt.Trigger, metrics.ArgoCDResultLookupError)
		b.log.Warn("argocd handler: find history by sha",
			zap.String("trigger", evt.Trigger),
			zap.String("argocd_app", evt.ArgoCDApp),
			zap.String("gitops_sha", evt.GitopsCommitSHA),
			zap.Error(err),
		)
		return
	}
	if entry == nil {
		b.metrics.RecordArgoCDNotification(evt.Trigger, metrics.ArgoCDResultUnmatched)
		b.log.Info("argocd notification unmatched, dropping",
			zap.String("trigger", evt.Trigger),
			zap.String("argocd_app", evt.ArgoCDApp),
			zap.String("gitops_sha", evt.GitopsCommitSHA),
		)
		return
	}

	switch evt.Trigger {
	case "on-sync-succeeded":
		// postArgoCDSuccess may drop silently when the history entry has
		// no slack handle to thread under — it records the right result
		// label (matched vs no_handle_skipped) from inside the function.
		b.postArgoCDSuccess(ctx, evt, entry)
	case "on-sync-failed":
		b.postArgoCDFailure(ctx, evt, entry, "DEPLOY FAILED", "sync failed")
		b.metrics.RecordArgoCDNotification(evt.Trigger, metrics.ArgoCDResultMatched)
	case "on-health-degraded":
		b.postArgoCDFailure(ctx, evt, entry, "HEALTH DEGRADED", "degraded")
		b.metrics.RecordArgoCDNotification(evt.Trigger, metrics.ArgoCDResultMatched)
	default:
		// on-sync-running is accepted by the receiver but never enqueued,
		// so reaching this branch implies a new trigger was added upstream
		// and we haven't wired handling for it yet. Log rather than panic.
		b.metrics.RecordArgoCDNotification(evt.Trigger, metrics.ArgoCDResultUnhandledTrigger)
		b.log.Warn("argocd handler: unhandled trigger, dropping",
			zap.String("trigger", evt.Trigger),
			zap.String("argocd_app", evt.ArgoCDApp),
		)
	}
}

// postArgoCDSuccess posts a quiet "synced and healthy" reply threaded
// under the original deploy message. If no original-message handle is
// stored on the history entry (a pre-phase-1 record, or a deploy path
// that did not persist the handle — notably ECR auto-deploy), the
// success message is dropped entirely rather than flattening into a
// standalone channel post.
func (b *Bot) postArgoCDSuccess(ctx context.Context, evt queue.ArgoCDNotificationEvent, entry *store.HistoryEntry) {
	if entry.SlackChannel == "" || entry.SlackMessageTS == "" {
		b.metrics.RecordArgoCDNotification(evt.Trigger, metrics.ArgoCDResultNoHandleSkipped)
		b.log.Debug("argocd success: no slack handle on history entry, dropping quiet reply",
			zap.String("app", entry.App),
			zap.String("env", entry.Environment),
			zap.String("tag", entry.Tag),
		)
		return
	}
	// entry.App is already the composite "app-env" FullName written by
	// handleApprove from PendingDeploy.App. Rendering it as-is avoids
	// the double-env artefact ("myapp-prod-prod") a naive `%s-%s`
	// concat would produce.
	text := fmt.Sprintf(
		":white_check_mark: ArgoCD synced *%s* at `%s` — healthy.",
		entry.App, entry.Tag,
	)
	b.postSlack(ctx, entry.SlackChannel, "argocd sync-succeeded reply",
		slack.MsgOptionText(text, false),
		slack.MsgOptionTS(entry.SlackMessageTS),
	)
	b.metrics.RecordArgoCDNotification(evt.Trigger, metrics.ArgoCDResultMatched)
}

// postArgoCDFailure posts an unmissable top-level alert for sync-failed
// and health-degraded events. Top-level (NOT threaded) is a deliberate
// design choice: failures must not be buried in whatever thread the
// original deploy lives in. Late-arriving failures (deploy > 2h old)
// are re-framed with a calmer "may not be related, investigate first"
// tone since rolling back a hours-old deploy is rarely the right fix.
//
// heading is the ALL-CAPS banner in the siren row ("DEPLOY FAILED" /
// "HEALTH DEGRADED"). stateDesc is the short "ArgoCD state" label
// ("sync failed" / "degraded") that appears below the banner.
func (b *Bot) postArgoCDFailure(
	ctx context.Context,
	evt queue.ArgoCDNotificationEvent,
	entry *store.HistoryEntry,
	heading, stateDesc string,
) {
	lateArrival := !entry.CompletedAt.IsZero() && time.Since(entry.CompletedAt) > lateArrivalThreshold

	// Post to the channel the original deploy lives in when possible,
	// so the failure lands in front of the same audience. Fall back to
	// the currently-configured deploy channel for records written
	// before the SlackChannel field existed.
	channel := entry.SlackChannel
	if channel == "" {
		channel = b.cfg.Load().Slack.DeployChannel
	}

	text := buildArgoCDFailureMessage(evt, entry, heading, stateDesc, lateArrival)

	// Resolve the permalink synchronously but tolerate failure: if
	// Slack's getPermalink is down, we post the message without a
	// back-link rather than dropping the alert.
	if link := b.resolveDeployPermalink(ctx, entry); link != "" {
		text += "\n" + link
		if entry.PRURL != "" && entry.PRNumber > 0 {
			text += fmt.Sprintf(" • <%s|PR #%d>", entry.PRURL, entry.PRNumber)
		}
	} else if entry.PRURL != "" && entry.PRNumber > 0 {
		text += fmt.Sprintf("\n<%s|PR #%d>", entry.PRURL, entry.PRNumber)
	}

	// Unthreaded on purpose: failures belong at the top level of the
	// deploy channel so they can't be missed by someone scanning the
	// channel view.
	b.postSlack(ctx, channel, "argocd failure notice", slack.MsgOptionText(text, false))

	// Phase 4: alongside the alarming status message, post a separate
	// top-level prompt carrying [Roll back] and [Dismiss] buttons. The
	// prompt self-suppresses on late arrivals and on deploys with no
	// prior known-good tag to roll back to.
	b.postArgoCDRollbackPrompt(ctx, channel, entry, lateArrival)
}

// buildArgoCDFailureMessage renders the alarming Slack message body. Kept
// separate from postArgoCDFailure so tests can assert against the output
// without needing a fake Slack client.
func buildArgoCDFailureMessage(
	evt queue.ArgoCDNotificationEvent,
	entry *store.HistoryEntry,
	heading, stateDesc string,
	lateArrival bool,
) string {
	var sb strings.Builder

	// entry.App carries the composite "app-env" FullName — do not
	// re-concatenate entry.Environment here, or the rendered label
	// comes out as "myapp-prod-prod".
	if lateArrival {
		// Calmer framing: this is almost certainly a runtime failure,
		// not a bad deploy. The subject is the app, not the deploy.
		fmt.Fprintf(&sb,
			":warning: *Health issue on previously-deployed `%s` (`%s`)*\n",
			entry.App, entry.Tag,
		)
		fmt.Fprintf(&sb,
			"Deployed %s by %s.\n\n",
			humanizeAge(time.Since(entry.CompletedAt)),
			slackMention(entry.RequesterID),
		)
	} else {
		// Alarming framing: sirens on both sides, ALL-CAPS banner,
		// requester explicitly pinged so they see it.
		fmt.Fprintf(&sb,
			":rotating_light::rotating_light: *%s* :rotating_light::rotating_light:\n",
			heading,
		)
		fmt.Fprintf(&sb,
			"*%s* — tag `%s` — deployed by %s\n\n",
			entry.App, entry.Tag,
			slackMention(entry.RequesterID),
		)
	}

	fmt.Fprintf(&sb, "ArgoCD state: *%s*\n", stateDesc)
	if evt.Message != "" {
		fmt.Fprintf(&sb,
			"ArgoCD message: _%s_\n",
			sanitize.SlackText(evt.Message, 500),
		)
	}

	if failing := parseAndFilterResources(evt.Resources); len(failing) > 0 {
		sb.WriteString("\n*Failing resources:*\n")
		for i, r := range failing {
			if i >= maxFailingResources {
				fmt.Fprintf(&sb, "_…and %d more_\n", len(failing)-maxFailingResources)
				break
			}
			fmt.Fprintf(&sb, "• :red_circle: `%s/%s` — *%s*", r.Kind, r.Name, r.HealthStatus)
			if r.HealthMessage != "" {
				fmt.Fprintf(&sb, ": %s", sanitize.SlackText(r.HealthMessage, 200))
			}
			sb.WriteString("\n")
		}
	}

	if lateArrival {
		sb.WriteString("\n:information_source: _This deploy is more than 2 hours old. " +
			"The current failure may not be caused by this deploy — investigate before rolling back._\n")
	}

	return sb.String()
}

// parseAndFilterResources decodes the raw resources JSON from the
// webhook payload and returns only the entries that actually report a
// non-healthy state. A malformed array is tolerated (returns nil) —
// resource detail is a nice-to-have; the message still carries the
// ArgoCD state and message without it.
func parseAndFilterResources(raw json.RawMessage) []argocdResource {
	if len(raw) == 0 {
		return nil
	}
	var all []argocdResource
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil
	}
	var failing []argocdResource
	for _, r := range all {
		// Filter out resources that don't report health (ConfigMaps,
		// Secrets, Services, etc. — these have no health status) and
		// resources that ARE healthy. Everything left is a real
		// candidate for the "failing resources" bullet list.
		if r.HealthStatus == "" || r.HealthStatus == "Healthy" {
			continue
		}
		failing = append(failing, r)
	}
	return failing
}

// humanizeAge returns a short, human-readable duration string like
// "3 hours ago" or "2 days ago" for the age of a history entry. Not
// precision-critical — this is meant to set a "how stale is this
// deploy" expectation for the on-call reading the alert.
func humanizeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments ago"
	case d < time.Hour:
		m := int(d.Minutes())
		return fmt.Sprintf("%d minute%s ago", m, plural(m))
	case d < 24*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%d hour%s ago", h, plural(h))
	default:
		days := int(d.Hours() / 24)
		return fmt.Sprintf("%d day%s ago", days, plural(days))
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// resolveDeployPermalink calls Slack's chat.getPermalink to build an
// mrkdwn-linked "Original deploy" reference. Returns an empty string
// when the history entry has no stored handle or when Slack's API
// fails, so callers can drop the link section without fanfare.
func (b *Bot) resolveDeployPermalink(ctx context.Context, entry *store.HistoryEntry) string {
	if entry.SlackChannel == "" || entry.SlackMessageTS == "" {
		return ""
	}
	permalink, err := b.slack.GetPermalinkContext(ctx, &slack.PermalinkParameters{
		Channel: entry.SlackChannel,
		Ts:      entry.SlackMessageTS,
	})
	if err != nil || permalink == "" {
		b.log.Debug("argocd failure: could not resolve deploy permalink, omitting link",
			zap.String("channel", entry.SlackChannel),
			zap.String("ts", entry.SlackMessageTS),
			zap.Error(err),
		)
		return ""
	}
	return fmt.Sprintf("<%s|Original deploy>", permalink)
}
