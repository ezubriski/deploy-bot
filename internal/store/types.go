package store

import "time"

const (
	StatePending = "pending"
	StateMerging = "merging"
	StateMerged  = "merged"
)

type PendingDeploy struct {
	// GitHubOrg and GitHubRepo identify the gitops repository the PR
	// lives in. Together with PRNumber they form the composite primary
	// key on the pending_deploys Postgres table, so the bot can manage
	// apps across multiple gitops repos in a single instance without
	// PR-number collisions. 1.x had a single-repo assumption baked
	// into its `pending:<pr>` Redis key; 3.0 carries the org/repo on
	// every row.
	//
	// For deploys sourced from the operator `github` config section,
	// these are populated from `config.GitHub.Org` / `config.GitHub.Repo`
	// at modal-submit time. For repo-discovered apps, they come from
	// the discovered app's SourceRepo.
	GitHubOrg   string    `json:"github_org"`
	GitHubRepo  string    `json:"github_repo"`
	App         string    `json:"app"`
	Environment string    `json:"environment"`
	Tag         string    `json:"tag"`
	PRNumber    int       `json:"pr_number"`
	PRURL       string    `json:"pr_url"`
	Requester   string    `json:"requester"`
	RequesterID string    `json:"requester_id"`
	ApproverID  string    `json:"approver_id"`
	Reason      string    `json:"reason"`
	RequestedAt time.Time `json:"requested_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	State       string    `json:"state"`

	// SlackChannel and SlackMessageTS identify the Slack message the bot
	// posted when announcing this deploy request. Populated by
	// SetSlackHandle after the approval post succeeds, and copied onto the
	// HistoryEntry at completion time so that follow-up notifications (e.g.
	// ArgoCD lifecycle updates) can reference the original message in
	// context — either as a threaded reply or as a permalink — after the
	// per-environment thread TTL has expired.
	SlackChannel   string `json:"slack_channel,omitempty"`
	SlackMessageTS string `json:"slack_message_ts,omitempty"`
}

// HistoryFromPending creates a HistoryEntry from a PendingDeploy, copying
// the fields that are always transferred. The caller sets EventType and any
// additional fields (e.g. GitopsCommitSHA for merges) on the returned value.
func HistoryFromPending(d *PendingDeploy, eventType string) HistoryEntry {
	return HistoryEntry{
		EventType:      eventType,
		App:            d.App,
		Environment:    d.Environment,
		Tag:            d.Tag,
		PRNumber:       d.PRNumber,
		PRURL:          d.PRURL,
		RequesterID:    d.RequesterID,
		CompletedAt:    time.Now(),
		SlackChannel:   d.SlackChannel,
		SlackMessageTS: d.SlackMessageTS,
	}
}

// HistoryEntry is an immutable record of a completed deployment event,
// stored in the `history` Postgres table for /deploy history queries,
// rollback-target resolution, and ArgoCD notification correlation.
type HistoryEntry struct {
	// GitHubOrg and GitHubRepo identify the gitops repository the
	// deploy targeted. Populated for every row inserted by 2.0+ code.
	// The 2.x → 3.0 data migration populates them from the top-level
	// `github` config section since 1.x is single-repo-by-definition.
	// NULL-tolerant in the schema; required for display and for
	// future cross-repo filtering.
	GitHubOrg   string `json:"github_org,omitempty"`
	GitHubRepo  string `json:"github_repo,omitempty"`
	EventType   string `json:"event_type"` // approved, rejected, expired, cancelled
	App         string `json:"app"`
	Environment string `json:"environment"`
	Tag         string `json:"tag"`
	PRNumber    int    `json:"pr_number"`
	PRURL       string `json:"pr_url"`
	// ApproverID is the Slack user ID of whoever clicked Approve
	// (for event_type='approved') or empty otherwise. 1.x did not
	// record this field at all; 3.0 adds it because Postgres makes
	// structured queries worthwhile and audit consumers asked for it.
	ApproverID  string    `json:"approver_id,omitempty"`
	RequesterID string    `json:"requester_id"` // Slack user ID for @mention
	CompletedAt time.Time `json:"completed_at"`

	// GitopsCommitSHA is the merge commit SHA in the gitops repo for an
	// approved/merged deploy. Empty for rejected, expired, or cancelled
	// entries (no merge happened). Used to correlate ArgoCD notifications
	// back to the deploy that produced the synced revision.
	GitopsCommitSHA string `json:"gitops_commit_sha,omitempty"`

	// SlackChannel and SlackMessageTS point to the Slack message that
	// announced the original deploy request, copied from the PendingDeploy
	// at completion time. Used by follow-up notifications that need to
	// reference the deploy in context after the entry has been pushed to
	// history.
	SlackChannel   string `json:"slack_channel,omitempty"`
	SlackMessageTS string `json:"slack_message_ts,omitempty"`
}
