package bot

import (
	"context"

	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/queue"
)

// handleArgoCDNotification is the worker-side dispatch for an ArgoCD
// lifecycle notification that the receiver enqueued onto argocd:events.
//
// Phase 2 implementation: this is intentionally a logging stub. The
// receiver-side webhook + dedupe + enqueue path is fully wired so
// operators can configure ArgoCD to start delivering notifications and
// confirm end-to-end plumbing, but no Slack post or rollback prompt
// happens yet — those land in phase 3 once the correlation logic
// (history lookup by GitopsCommitSHA) and the alarming-failure message
// shape are in place.
//
// Logging at info level so phase 2 deployments produce a visible
// breadcrumb per accepted notification, which is the easiest way to
// validate the wiring without needing the full handler.
func (b *Bot) handleArgoCDNotification(ctx context.Context, evt queue.ArgoCDNotificationEvent) {
	_ = ctx // reserved for phase 3 — history lookups, slack posts, dedupe state writes
	b.log.Info("argocd notification received (phase 2 stub — no slack post yet)",
		zap.String("trigger", evt.Trigger),
		zap.String("argocd_app", evt.ArgoCDApp),
		zap.String("gitops_sha", evt.GitopsCommitSHA),
		zap.String("sync_status", evt.SyncStatus),
		zap.String("health_status", evt.HealthStatus),
		zap.String("phase", evt.Phase),
	)
}
