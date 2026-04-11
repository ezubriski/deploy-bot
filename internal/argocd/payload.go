// Package argocd implements the receiver-side webhook endpoint for inbound
// ArgoCD lifecycle notifications. It accepts a small JSON payload from the
// argocd-notifications controller's webhook service, validates a shared
// secret header, dedupes against a Redis-backed cache, and enqueues an
// ArgoCDNotificationEvent on the argocd:events stream for the worker to
// process.
//
// The handler does no Slack posting and no GitHub lookups — those happen on
// the worker side in internal/bot. Keeping the receiver hot path narrow
// matches the ECR webhook precedent and the argocd-notifications retry
// model: the controller only retries on 5xx and network errors with no
// dead-letter queue, so returning 2xx fast is the right contract.
package argocd

import "encoding/json"

// WebhookPayload is the JSON shape we expect from the
// argocd-notifications-controller webhook service. The body is fully
// controlled by the template we ship in deploy/argocd-notifications/
// templates.yaml — see that file for the canonical Go-template that
// produces this exact shape.
//
// We deliberately model only fields we plan to use. Unknown fields are
// ignored on decode (encoding/json default), so additive changes to the
// upstream template do not break receiver parsing.
type WebhookPayload struct {
	// Trigger is the argocd-notifications trigger name (e.g.
	// "on-sync-succeeded", "on-sync-failed", "on-health-degraded").
	// Sourced from {{.context.eventType}} in the template.
	Trigger string `json:"trigger"`

	// ArgoCDApp is .app.metadata.name. Used for log breadcrumbs and as a
	// fallback identifier when GitopsCommitSHA does not match any history
	// entry.
	ArgoCDApp string `json:"argocdApp"`

	// Namespace is .app.metadata.namespace.
	Namespace string `json:"namespace,omitempty"`

	// RepoURL is .app.spec.source.repoURL — the gitops repo. Used as a
	// sanity check during correlation: if it does not match the configured
	// gitops repo we know we are looking at a different ArgoCD app and the
	// SHA collision (if any) is coincidental.
	RepoURL string `json:"repoURL,omitempty"`

	// Revision is .app.status.sync.revision — the ArgoCD-reported "current
	// desired" revision. Usually equal to SyncResultRevision but can lag
	// during in-flight syncs. Kept for diagnostics; correlation uses
	// SyncResultRevision.
	Revision string `json:"revision,omitempty"`

	// SyncResultRevision is .app.status.operationState.syncResult.revision
	// — the SHA that was actually applied. This is the load-bearing
	// correlation field.
	SyncResultRevision string `json:"syncResultRevision"`

	// SyncStatus is .app.status.sync.status (Synced, OutOfSync, Unknown).
	SyncStatus string `json:"syncStatus,omitempty"`

	// HealthStatus is .app.status.health.status (Healthy, Degraded,
	// Progressing, Missing, Unknown, Suspended).
	HealthStatus string `json:"healthStatus,omitempty"`

	// Phase is .app.status.operationState.phase (Running, Succeeded, Error,
	// Failed, Terminating).
	Phase string `json:"phase,omitempty"`

	// Message is .app.status.operationState.message — human-readable
	// failure reason on a failed sync.
	Message string `json:"message,omitempty"`

	// FinishedAt is .app.status.operationState.finishedAt as an opaque
	// string (ArgoCD emits RFC3339). The receiver does not parse it; the
	// worker uses it to render "happened N minutes ago" framing.
	FinishedAt string `json:"finishedAt,omitempty"`

	// Resources is .app.status.resources rendered into the template body
	// as an array of objects (kind/name/namespace/syncStatus/healthStatus/
	// healthMessage). Captured here as raw JSON to keep this package
	// decoupled from the upstream Application schema; the worker renders
	// the per-resource detail block on degraded/failed events without
	// re-parsing in this package.
	Resources json.RawMessage `json:"resources,omitempty"`
}

// IsRecognizedTrigger reports whether the trigger name is one of the four
// the bot subscribes to. Unknown triggers are still accepted by the
// webhook (returning 200) but are dropped without enqueueing — operators
// can add new subscriptions in the ConfigMap without coordinating a bot
// release, and unknown ones become a no-op rather than a 4xx that would
// cause the controller to log spam.
func IsRecognizedTrigger(trigger string) bool {
	switch trigger {
	case "on-sync-succeeded",
		"on-sync-failed",
		"on-health-degraded",
		"on-sync-running": // accepted but currently dropped — see CLAUDE.md / docs
		return true
	}
	return false
}
