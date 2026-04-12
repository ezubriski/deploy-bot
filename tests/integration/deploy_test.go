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
	tag := pickTagFor(t, env.app)
	purgeStaleBranch(t, env.app, tag)

	injectDeployRequestWithTag(t, tag, "integration test: approve path")

	prNumber := waitForPRWithTag(t, tag)
	t.Cleanup(func() { cleanupPRWithTag(t, prNumber, tag) })

	t.Logf("deploy PR created: #%d (tag %s)", prNumber, tag)

	// Verify the pending deploy record is well-formed.
	d, err := env.store.Get(context.Background(), env.cfg.GitHub.Org, env.cfg.GitHub.Repo, prNumber)
	if err != nil || d == nil {
		t.Fatalf("expected pending deploy in Redis for PR #%d", prNumber)
	}
	if d.App != env.app {
		t.Errorf("pending deploy app = %q, want %q", d.App, env.app)
	}
	if d.Tag != tag {
		t.Errorf("pending deploy tag = %q, want %q", d.Tag, tag)
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
		d, _ := env.store.Get(context.Background(), env.cfg.GitHub.Org, env.cfg.GitHub.Repo, prNumber)
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
		_ = env.store.ReleaseLock(context.Background(), env.environment, env.app)
	}

	// History should contain an approved entry for this deploy.
	var historyEntries []store.HistoryEntry
	if !poll(t, 5*time.Second, func() bool {
		entries, err := env.store.GetHistory(context.Background(), "", 20)
		if err != nil {
			return false
		}
		historyEntries = entries
		for _, e := range entries {
			if e.App == env.app && e.Tag == tag && e.EventType == audit.EventApproved && e.PRNumber == prNumber {
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
	tag := pickTagFor(t, env.app)
	purgeStaleBranch(t, env.app, tag)

	injectDeployRequestWithTag(t, tag, "integration test: reject path")

	prNumber := waitForPRWithTag(t, tag)
	t.Cleanup(func() { cleanupPRWithTag(t, prNumber, tag) })

	t.Logf("deploy PR created: #%d (tag %s)", prNumber, tag)

	// Reject by submitting the rejection modal directly.
	injectRejectSubmit(t, prNumber, "rejected by integration test")

	// Poll until the pending deploy is removed from Redis.
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), env.cfg.GitHub.Org, env.cfg.GitHub.Repo, prNumber)
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
	var historyEntries []store.HistoryEntry
	if !poll(t, 5*time.Second, func() bool {
		entries, err := env.store.GetHistory(context.Background(), "", 20)
		if err != nil {
			return false
		}
		historyEntries = entries
		for _, e := range entries {
			if e.App == env.app && e.Tag == tag && e.EventType == audit.EventRejected && e.PRNumber == prNumber {
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
	tag := pickTagFor(t, env.app)
	purgeStaleBranch(t, env.app, tag)

	injectDeployRequestWithTag(t, tag, "integration test: lock contention first")

	firstPR := waitForPRWithTag(t, tag)
	t.Cleanup(func() { cleanupPRWithTag(t, firstPR, tag) })

	t.Logf("first deploy PR: #%d (tag %s)", firstPR, tag)

	// Attempt a second deploy while the first is pending; lock must block it.
	injectDeployRequestWithTag(t, tag, "integration test: lock contention second")

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
		d, _ := env.store.Get(context.Background(), env.cfg.GitHub.Org, env.cfg.GitHub.Repo, firstPR)
		return d == nil
	})
}

func TestCancelDeploy(t *testing.T) {
	resetAppState(t)
	tag := pickTagFor(t, env.app)
	purgeStaleBranch(t, env.app, tag)

	injectDeployRequestWithTag(t, tag, "integration test: cancel path")

	prNumber := waitForPRWithTag(t, tag)
	t.Cleanup(func() { cleanupPRWithTag(t, prNumber, tag) })
	t.Logf("deploy PR created: #%d (tag %s)", prNumber, tag)

	injectSlashCommand(t, fmt.Sprintf("cancel %d", prNumber))

	// Poll until the pending deploy is removed from Redis.
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), env.cfg.GitHub.Org, env.cfg.GitHub.Repo, prNumber)
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
		entries, err := env.store.GetHistory(context.Background(), "", 20)
		if err != nil {
			return false
		}
		historyEntries = entries
		for _, e := range entries {
			if e.App == env.app && e.Tag == tag && e.EventType == audit.EventCancelled && e.PRNumber == prNumber {
				return true
			}
		}
		return false
	}) {
		t.Errorf("expected cancelled history entry for PR #%d, got %+v", prNumber, historyEntries)
	}
}

// TestRollback verifies the rollback flow end-to-end:
//  1. Deploy and approve tagA (becomes previous), then deploy and approve tagB (becomes current).
//  2. Inject /deploy rollback — the bot reads history, finds tagA as the rollback
//     target, and attempts to open a Slack modal (fails silently without a real TriggerID).
//  3. Inject the modal submission for tagA directly, simulating the user submitting
//     the pre-filled modal.
//  4. Approve the rollback deploy and verify it completes.
func TestRollback(t *testing.T) {
	resetAppState(t)

	// Pick two distinct tags, neither currently deployed.
	tagA := pickTagFor(t, env.app)
	tagB := pickTagFor(t, env.app, tagA)
	t.Logf("rollback test: tagA=%s tagB=%s", tagA, tagB)

	// Step 1a: deploy tagA and approve it.
	purgeStaleBranch(t, env.app, tagA)
	injectDeployRequestWithTag(t, tagA, "integration test: rollback setup — first deploy")
	firstPR := waitForPRWithTag(t, tagA)
	t.Cleanup(func() { cleanupPRWithTag(t, firstPR, tagA) })
	injectApprove(t, firstPR)
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), env.cfg.GitHub.Org, env.cfg.GitHub.Repo, firstPR)
		return d == nil
	}) {
		t.Fatal("timed out waiting for first setup deploy to complete")
	}
	// Delete the merged branch so the rollback redeploy can create a fresh one.
	if err := env.ghClient.DeleteBranch(context.Background(), deployBranch(env.environment, env.app, tagA)); err != nil {
		t.Fatalf("delete setup branch: %v", err)
	}

	// Step 1b: deploy tagB and approve it — this becomes "current".
	purgeStaleBranch(t, env.app, tagB)
	injectDeployRequestWithTag(t, tagB, "integration test: rollback setup — second deploy")
	secondPR := waitForPRWithTag(t, tagB)
	t.Cleanup(func() { cleanupPRWithTag(t, secondPR, tagB) })
	injectApprove(t, secondPR)
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), env.cfg.GitHub.Org, env.cfg.GitHub.Repo, secondPR)
		return d == nil
	}) {
		t.Fatal("timed out waiting for second setup deploy to complete")
	}

	// Step 2: inject the rollback slash command. The bot validates deployer
	// membership, reads history, and calls openDeployModal — which fails
	// silently without a real TriggerID.
	injectSlashCommand(t, "rollback "+env.app)

	// Step 3: inject the modal submission for tagA. Enqueued immediately after
	// the slash command; Redis Streams FIFO ordering ensures the rollback
	// command is processed first.
	injectDeployRequestWithTag(t, tagA, "integration test: rollback deploy")

	rollbackPR := waitForPRWithTag(t, tagA)
	t.Cleanup(func() { cleanupPRWithTag(t, rollbackPR, tagA) })
	t.Logf("rollback PR: #%d (tag %s)", rollbackPR, tagA)

	// Step 4: approve and verify.
	injectApprove(t, rollbackPR)
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), env.cfg.GitHub.Org, env.cfg.GitHub.Repo, rollbackPR)
		return d == nil
	}) {
		t.Fatal("timed out waiting for rollback deploy to complete")
	}

	var historyEntries []store.HistoryEntry
	if !poll(t, 5*time.Second, func() bool {
		entries, _ := env.store.GetHistory(context.Background(), "", 20)
		historyEntries = entries
		for _, e := range entries {
			if e.App == env.app && e.Tag == tagA && e.EventType == audit.EventApproved && e.PRNumber == rollbackPR {
				return true
			}
		}
		return false
	}) {
		t.Errorf("expected approved history for rollback PR #%d tag %s, got %+v", rollbackPR, tagA, historyEntries)
	}
}
