//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/store"
)

func TestDeployAndApprove(t *testing.T) {
	resetAppState(t)

	injectDeployRequest(t, "integration test: approve path")

	prNumber := waitForPR(t)
	t.Cleanup(func() { cleanupPR(t, prNumber) })

	t.Logf("deploy PR created: #%d", prNumber)

	// Verify the pending deploy record is well-formed.
	d, err := env.store.Get(context.Background(), prNumber)
	if err != nil || d == nil {
		t.Fatalf("expected pending deploy in Redis for PR #%d", prNumber)
	}
	if d.App != env.app {
		t.Errorf("pending deploy app = %q, want %q", d.App, env.app)
	}
	if d.Tag != env.tag {
		t.Errorf("pending deploy tag = %q, want %q", d.Tag, env.tag)
	}
	if d.ApproverID != env.approverID {
		t.Errorf("pending deploy approverID = %q, want %q", d.ApproverID, env.approverID)
	}
	if d.PRURL == "" {
		t.Error("pending deploy PRURL is empty")
	}

	// Approve.
	injectApprove(t, prNumber)

	// Poll until the pending deploy is removed from Redis (merge complete).
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), prNumber)
		return d == nil
	}) {
		t.Fatal("timed out waiting for deploy to be approved and merged")
	}

	// App lock must be released.
	locked, err := env.store.IsLocked(context.Background(), env.app)
	if err != nil {
		t.Fatalf("check lock: %v", err)
	}
	if locked {
		t.Error("app lock should be released after merge")
		_ = env.store.ReleaseLock(context.Background(), env.app) // repair for next test
	}

	// History should contain an approved entry for this deploy.
	// PushHistory is called after Delete in the handler, so poll briefly.
	var historyEntries []store.HistoryEntry
	if !poll(t, 5*time.Second, func() bool {
		entries, err := env.store.GetHistory(context.Background(), 20)
		if err != nil {
			return false
		}
		historyEntries = entries
		for _, e := range entries {
			if e.App == env.app && e.Tag == env.tag && e.EventType == audit.EventApproved && e.PRNumber == prNumber {
				return true
			}
		}
		return false
	}) {
		t.Errorf("expected approved history entry for PR #%d, got %+v", prNumber, historyEntries)
	}
}

func TestDeployAndReject(t *testing.T) {
	resetAppState(t)

	injectDeployRequest(t, "integration test: reject path")

	prNumber := waitForPR(t)
	t.Cleanup(func() { cleanupPR(t, prNumber) })

	t.Logf("deploy PR created: #%d", prNumber)

	// Reject by submitting the rejection modal directly (skip the button click
	// which only opens the modal on the Slack side).
	injectRejectSubmit(t, prNumber, "rejected by integration test")

	// Poll until the pending deploy is removed from Redis.
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), prNumber)
		return d == nil
	}) {
		t.Fatal("timed out waiting for deploy to be rejected")
	}

	// App lock must be released.
	locked, _ := env.store.IsLocked(context.Background(), env.app)
	if locked {
		t.Error("app lock should be released after rejection")
		_ = env.store.ReleaseLock(context.Background(), env.app)
	}

	// History should contain a rejected entry.
	// PushHistory is called after Delete in the handler, so poll briefly.
	var historyEntries []store.HistoryEntry
	if !poll(t, 5*time.Second, func() bool {
		entries, err := env.store.GetHistory(context.Background(), 20)
		if err != nil {
			return false
		}
		historyEntries = entries
		for _, e := range entries {
			if e.App == env.app && e.Tag == env.tag && e.EventType == audit.EventRejected && e.PRNumber == prNumber {
				return true
			}
		}
		return false
	}) {
		t.Errorf("expected rejected history entry for PR #%d, got %+v", prNumber, historyEntries)
	}
}

func TestDeployLockPreventsSecondDeploy(t *testing.T) {
	resetAppState(t)

	injectDeployRequest(t, "integration test: lock contention first")

	firstPR := waitForPR(t)
	t.Cleanup(func() { cleanupPR(t, firstPR) })

	t.Logf("first deploy PR: #%d", firstPR)

	// Attempt a second deploy for the same app while the first is pending.
	// The worker should DM the requester that a deploy is already in progress
	// and must NOT create a second PR.
	injectDeployRequest(t, "integration test: lock contention second")

	// Give the worker time to process the second request.
	time.Sleep(5 * time.Second)

	// There should still be exactly one pending deploy for this app.
	deploys, err := env.store.GetAll(context.Background())
	if err != nil {
		t.Fatalf("get all deploys: %v", err)
	}
	var appDeploys []*store.PendingDeploy
	for _, d := range deploys {
		if d.App == env.app {
			appDeploys = append(appDeploys, d)
		}
	}
	if len(appDeploys) != 1 {
		t.Errorf("expected 1 pending deploy for %s, got %d", env.app, len(appDeploys))
	}

	// Clean up the first deploy.
	injectApprove(t, firstPR)
	poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), firstPR)
		return d == nil
	})
}
