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
}
