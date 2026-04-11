package queue

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack/socketmode"
)

// EventTypeArgoCDNotification is the event type for ArgoCD lifecycle webhook
// notifications (sync-succeeded, sync-failed, health-degraded). Defined as a
// socketmode.EventType so it can be carried by the same envelope, buffer, and
// worker infrastructure as Slack and ECR events.
const EventTypeArgoCDNotification socketmode.EventType = "argocd_notification"

// ArgoCDNotificationEvent is the payload enqueued for an ArgoCD lifecycle
// notification. The receiver-side webhook handler parses an incoming
// argocd-notifications-controller payload, validates the signature, dedupes
// against the per-revision Redis cache, and writes one of these onto the
// argocd:events stream.
//
// The shape is intentionally narrow: it carries only the fields the worker
// needs to correlate the signal back to a deploy-bot history entry and
// produce a Slack message. The full upstream payload (resource list, etc)
// is preserved as raw JSON in Resources for the worker to render
// resource-level details on degraded/failed events without re-parsing the
// upstream schema in this package.
type ArgoCDNotificationEvent struct {
	// Trigger is the argocd-notifications trigger name (e.g.
	// "on-sync-succeeded", "on-sync-failed", "on-health-degraded").
	Trigger string `json:"trigger"`

	// ArgoCDApp is the ArgoCD Application metadata.name as reported in the
	// payload. Used for log breadcrumbs and unmatched-notification posts.
	ArgoCDApp string `json:"argocd_app"`

	// Namespace is the ArgoCD Application metadata.namespace.
	Namespace string `json:"namespace,omitempty"`

	// RepoURL is the gitops repo URL the application syncs from.
	RepoURL string `json:"repo_url,omitempty"`

	// GitopsCommitSHA is the synced revision (.app.status.operationState.
	// syncResult.revision). This is the load-bearing field for correlation
	// — the worker matches it against HistoryEntry.GitopsCommitSHA captured
	// at merge time.
	GitopsCommitSHA string `json:"gitops_commit_sha"`

	// SyncStatus is the ArgoCD sync status string (Synced, OutOfSync, Unknown).
	SyncStatus string `json:"sync_status,omitempty"`

	// HealthStatus is the ArgoCD health status string (Healthy, Degraded,
	// Progressing, Missing, Unknown, Suspended).
	HealthStatus string `json:"health_status,omitempty"`

	// Phase is the sync operation phase (Running, Succeeded, Error, Failed).
	Phase string `json:"phase,omitempty"`

	// Message is the operationState.message from ArgoCD, often containing
	// the human-readable failure reason.
	Message string `json:"message,omitempty"`

	// Resources is the raw JSON of the resources array from the webhook
	// payload, preserved verbatim so the worker can render per-resource
	// status on health-degraded / sync-failed without coupling this package
	// to the upstream schema. May be empty.
	Resources []byte `json:"resources,omitempty"`

	// ReceivedAt is when the receiver accepted the webhook. Used by the
	// worker to detect late-arriving degradations and reframe the rollback
	// prompt accordingly.
	ReceivedAt time.Time `json:"received_at"`
}

// NewArgoCDNotificationEvent wraps an ArgoCDNotificationEvent in a
// socketmode.Event so it can be enqueued and buffered using the same path
// as Slack events.
func NewArgoCDNotificationEvent(evt ArgoCDNotificationEvent) socketmode.Event {
	return socketmode.Event{
		Type: EventTypeArgoCDNotification,
		Data: evt,
	}
}

// EnqueueArgoCD appends an event to the ArgoCD stream.
func EnqueueArgoCD(ctx context.Context, rdb *redis.Client, evt socketmode.Event) error {
	return EnqueueTo(ctx, rdb, StreamKeyArgoCD, evt)
}
