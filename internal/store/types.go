package store

import "time"

const (
	StatePending = "pending"
	StateMerging = "merging"
	StateMerged  = "merged"
)

type PendingDeploy struct {
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

	// GitopsCommitSHA is the merge commit SHA in the gitops repo, populated
	// when the deploy PR is merged. Used to correlate ArgoCD notifications
	// (which carry the synced revision) back to the deploy that produced it.
	// Empty until merge.
	GitopsCommitSHA string `json:"gitops_commit_sha,omitempty"`

	// SlackChannel and SlackMessageTS identify the Slack message the bot
	// posted when announcing this deploy request. They are persisted so that
	// follow-up notifications (e.g. ArgoCD lifecycle updates) can post in
	// context — either as a threaded reply or as a permalink reference —
	// after the per-environment thread TTL has expired.
	SlackChannel   string `json:"slack_channel,omitempty"`
	SlackMessageTS string `json:"slack_message_ts,omitempty"`
}

// HistoryEntry is an immutable record of a completed deployment event,
// stored in a Redis list for /deploy history queries.
type HistoryEntry struct {
	EventType   string    `json:"event_type"` // approved, rejected, expired, cancelled
	App         string    `json:"app"`
	Environment string    `json:"environment"`
	Tag         string    `json:"tag"`
	PRNumber    int       `json:"pr_number"`
	PRURL       string    `json:"pr_url"`
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
