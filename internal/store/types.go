package store

import "time"

const (
	StatePending = "pending"
	StateMerging = "merging"
	StateMerged  = "merged"
)

type PendingDeploy struct {
	App         string    `json:"app"`
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
