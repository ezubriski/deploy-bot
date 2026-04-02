//go:build integration

package integration

import (
	"context"
	"fmt"
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
	locked, err := env.store.IsLocked(context.Background(), env.environment, env.app)
	if err != nil {
		t.Fatalf("check lock: %v", err)
	}
	if locked {
		t.Error("app lock should be released after merge")
		_ = env.store.ReleaseLock(context.Background(), env.environment, env.app) // repair for next test
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
	locked, _ := env.store.IsLocked(context.Background(), env.environment, env.app)
	if locked {
		t.Error("app lock should be released after rejection")
		_ = env.store.ReleaseLock(context.Background(), env.environment, env.app)
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

func TestCancelDeploy(t *testing.T) {
	resetAppState(t)

	injectDeployRequest(t, "integration test: cancel path")

	prNumber := waitForPR(t)
	t.Cleanup(func() { cleanupPR(t, prNumber) })
	t.Logf("deploy PR created: #%d", prNumber)

	injectSlashCommand(t, fmt.Sprintf("cancel %d", prNumber))

	// Poll until the pending deploy is removed from Redis.
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), prNumber)
		return d == nil
	}) {
		t.Fatal("timed out waiting for cancel to complete")
	}

	// App lock must be released.
	locked, _ := env.store.IsLocked(context.Background(), env.environment, env.app)
	if locked {
		t.Error("app lock should be released after cancel")
		_ = env.store.ReleaseLock(context.Background(), env.environment, env.app)
	}

	// History should contain a cancelled entry.
	var historyEntries []store.HistoryEntry
	if !poll(t, 5*time.Second, func() bool {
		entries, err := env.store.GetHistory(context.Background(), 20)
		if err != nil {
			return false
		}
		historyEntries = entries
		for _, e := range entries {
			if e.App == env.app && e.Tag == env.tag && e.EventType == audit.EventCancelled && e.PRNumber == prNumber {
				return true
			}
		}
		return false
	}) {
		t.Errorf("expected cancelled history entry for PR #%d, got %+v", prNumber, historyEntries)
	}
}

// TestRollback verifies the rollback flow end-to-end:
//  1. Deploy and approve tag A, then deploy and approve tag B (B becomes current).
//  2. Inject /deploy rollback — the bot reads history, finds A as the rollback
//     target, and attempts to open a Slack modal (which fails silently since
//     there is no real TriggerID in the injected event, but that is expected).
//  3. Inject the modal submission for tag A directly, simulating the user
//     submitting the pre-filled modal that the bot would have shown.
//  4. Approve the rollback deploy and verify it completes.
func TestRollback(t *testing.T) {
	resetAppState(t)

	const rollbackTag = "v1.24.0" // older tag; will be "previous" after we deploy env.tag

	// Step 1a: deploy rollbackTag and approve it.
	injectDeployRequestWithTag(t, rollbackTag, "integration test: rollback setup — first deploy")
	firstPR := waitForPRWithTag(t, rollbackTag)
	t.Cleanup(func() { cleanupPRWithTag(t, firstPR, rollbackTag) })
	injectApprove(t, firstPR)
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), firstPR)
		return d == nil
	}) {
		t.Fatal("timed out waiting for first setup deploy to complete")
	}
	// The merged PR's branch persists in GitHub (no auto-delete). Delete it now
	// so step 3 can create a new PR for the same tag without a ref conflict.
	if err := env.ghClient.DeleteBranch(context.Background(), deployBranch(env.environment, env.app, rollbackTag)); err != nil {
		t.Fatalf("delete setup branch: %v", err)
	}

	// Step 1b: deploy env.tag and approve it — this becomes "current".
	injectDeployRequest(t, "integration test: rollback setup — second deploy")
	secondPR := waitForPR(t)
	t.Cleanup(func() { cleanupPR(t, secondPR) })
	injectApprove(t, secondPR)
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), secondPR)
		return d == nil
	}) {
		t.Fatal("timed out waiting for second setup deploy to complete")
	}

	// Step 2: inject the rollback slash command. The bot validates deployer
	// membership, reads history, and calls openDeployModal — which fails
	// silently without a real TriggerID.
	injectSlashCommand(t, "rollback "+env.app)

	// Step 3: inject the modal submission for the rollback tag. This simulates
	// the user submitting the pre-filled modal. We enqueue it immediately after
	// the slash command; Redis Streams FIFO ordering ensures the rollback
	// command is processed first.
	injectDeployRequestWithTag(t, rollbackTag, "integration test: rollback deploy")

	rollbackPR := waitForPRWithTag(t, rollbackTag)
	t.Cleanup(func() { cleanupPRWithTag(t, rollbackPR, rollbackTag) })
	t.Logf("rollback PR: #%d (tag %s)", rollbackPR, rollbackTag)

	// Step 4: approve and verify.
	injectApprove(t, rollbackPR)
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), rollbackPR)
		return d == nil
	}) {
		t.Fatal("timed out waiting for rollback deploy to complete")
	}

	var historyEntries []store.HistoryEntry
	if !poll(t, 5*time.Second, func() bool {
		entries, _ := env.store.GetHistory(context.Background(), 20)
		historyEntries = entries
		for _, e := range entries {
			if e.App == env.app && e.Tag == rollbackTag && e.EventType == audit.EventApproved && e.PRNumber == rollbackPR {
				return true
			}
		}
		return false
	}) {
		t.Errorf("expected approved history for rollback PR #%d tag %s, got %+v", rollbackPR, rollbackTag, historyEntries)
	}
}
