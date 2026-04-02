//go:build integration

package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/ezubriski/deploy-bot/internal/bot"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/store"
)

// buildDeployEvent constructs a deploy modal submission event without
// enqueuing it. Useful for concurrent enqueue scenarios where t.Fatalf
// cannot be called from a goroutine.
func buildDeployEvent(app, tag, reason string) socketmode.Event {
	return socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slack.InteractionCallback{
			Type: slack.InteractionTypeViewSubmission,
			View: slack.View{
				CallbackID: bot.ModalCallbackDeploy,
				State: &slack.ViewState{
					Values: map[string]map[string]slack.BlockAction{
						bot.BlockApp: {
							bot.ActionApp: {SelectedOption: slack.OptionBlockObject{Value: app}},
						},
						bot.BlockTag: {
							bot.ActionTag: {SelectedOption: slack.OptionBlockObject{}},
						},
						bot.BlockTagManual: {
							bot.ActionTagManual: {Value: tag},
						},
						bot.BlockReason: {
							bot.ActionReason: {Value: reason},
						},
						bot.BlockApprover: {
							bot.ActionApprover: {SelectedUser: env.approverID},
						},
					},
				},
			},
			User: slack.User{ID: env.requesterID},
		},
	}
}

// enqueueConcurrent fires n enqueue operations simultaneously for the given
// events and returns any errors.
func enqueueConcurrent(events []socketmode.Event) []error {
	start := make(chan struct{})
	errs := make(chan error, len(events))

	var wg sync.WaitGroup
	for _, evt := range events {
		wg.Add(1)
		go func(e socketmode.Event) {
			defer wg.Done()
			<-start
			errs <- queue.Enqueue(context.Background(), env.store.Redis(), e)
		}(evt)
	}

	close(start) // release all goroutines at once
	wg.Wait()
	close(errs)

	var out []error
	for err := range errs {
		if err != nil {
			out = append(out, err)
		}
	}
	return out
}

// TestConcurrentLockContention fires n simultaneous deploy requests for the
// same app and verifies that exactly one PR is created. The other n-1
// requests must be rejected by the deploy lock without creating additional
// PRs or corrupting Redis state.
func TestConcurrentLockContention(t *testing.T) {
	const n = 10
	resetAppState(t)

	events := make([]socketmode.Event, n)
	for i := range events {
		events[i] = buildDeployEvent(env.app, env.tag,
			fmt.Sprintf("stress: concurrent request %d", i))
	}

	if errs := enqueueConcurrent(events); len(errs) > 0 {
		t.Fatalf("enqueue errors: %v", errs)
	}

	// Wait for the first PR to appear.
	var firstPR int
	if !poll(t, 60*time.Second, func() bool {
		deploys, _ := env.store.GetAll(context.Background())
		for _, d := range deploys {
			if d.App == env.app {
				firstPR = d.PRNumber
				return true
			}
		}
		return false
	}) {
		t.Fatal("timed out waiting for any deploy PR to appear")
	}
	t.Cleanup(func() { cleanupPR(t, firstPR) })

	// Give the worker time to process the remaining n-1 events. Rejected
	// requests (lock held) are fast — no GitHub API calls — so 15s is generous.
	time.Sleep(15 * time.Second)

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
		t.Errorf("expected exactly 1 pending deploy for %s after %d concurrent requests, got %d",
			env.app, n, len(appDeploys))
	}

	t.Logf("lock held correctly: 1 PR (#%d) from %d concurrent requests", firstPR, n)

	// Verify lock is held and Redis state is consistent.
	locked, err := env.store.IsLocked(context.Background(), env.environment, env.app)
	if err != nil {
		t.Fatalf("check lock: %v", err)
	}
	if !locked {
		t.Error("expected deploy lock to still be held")
	}

	// Clean up.
	injectApprove(t, firstPR)
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), firstPR)
		return d == nil
	}) {
		t.Fatal("timed out waiting for approved deploy to complete")
	}
}

// TestConcurrentDifferentApps fires simultaneous deploy requests for n
// distinct apps and verifies all n PRs are created and complete without
// interfering with each other.
func TestConcurrentDifferentApps(t *testing.T) {
	apps := []string{"nginx-06", "nginx-07", "nginx-08", "nginx-09", "nginx-10"}

	for _, app := range apps {
		resetAppStateFor(t, app)
		purgeStaleBranch(t, app, env.tag)
	}

	events := make([]socketmode.Event, len(apps))
	for i, app := range apps {
		events[i] = buildDeployEvent(app, env.tag,
			fmt.Sprintf("stress: concurrent deploy of %s", app))
	}

	if errs := enqueueConcurrent(events); len(errs) > 0 {
		t.Fatalf("enqueue errors: %v", errs)
	}

	// Poll until all apps have a pending deploy.
	prByApp := make(map[string]int)
	if !poll(t, 120*time.Second, func() bool {
		deploys, _ := env.store.GetAll(context.Background())
		for _, d := range deploys {
			for _, app := range apps {
				if d.App == app {
					prByApp[app] = d.PRNumber
				}
			}
		}
		return len(prByApp) == len(apps)
	}) {
		var found []string
		for app := range prByApp {
			found = append(found, app)
		}
		t.Fatalf("only %d of %d apps got PRs; found: %v", len(prByApp), len(apps), found)
	}

	t.Logf("all %d apps got PRs: %v", len(apps), prByApp)

	for app, pr := range prByApp {
		app, pr := app, pr
		t.Cleanup(func() { cleanupPRForApp(t, pr, app, env.tag) })
		injectApprove(t, pr)
	}

	// Wait for all deploys to complete.
	if !poll(t, 120*time.Second, func() bool {
		for _, pr := range prByApp {
			if d, _ := env.store.Get(context.Background(), pr); d != nil {
				return false
			}
		}
		return true
	}) {
		t.Fatal("timed out waiting for all concurrent deploys to complete")
	}

	t.Logf("all %d concurrent deploys completed", len(apps))
}

// TestMultiWorker_LockContention runs the same lock-contention scenario as
// TestConcurrentLockContention but with two workers racing to process events.
// This verifies that the deploy lock holds correctly even when events land on
// different worker goroutines simultaneously.
func TestMultiWorker_LockContention(t *testing.T) {
	const n = 10
	startExtraWorker(t, "stress-worker-2")
	resetAppState(t)

	events := make([]socketmode.Event, n)
	for i := range events {
		events[i] = buildDeployEvent(env.app, env.tag,
			fmt.Sprintf("multi-worker: concurrent request %d", i))
	}

	if errs := enqueueConcurrent(events); len(errs) > 0 {
		t.Fatalf("enqueue errors: %v", errs)
	}

	var firstPR int
	if !poll(t, 60*time.Second, func() bool {
		deploys, _ := env.store.GetAll(context.Background())
		for _, d := range deploys {
			if d.App == env.app {
				firstPR = d.PRNumber
				return true
			}
		}
		return false
	}) {
		t.Fatal("timed out waiting for any deploy PR to appear")
	}
	t.Cleanup(func() { cleanupPR(t, firstPR) })

	// Allow time for both workers to drain remaining events.
	time.Sleep(15 * time.Second)

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
		t.Errorf("expected exactly 1 pending deploy for %s with 2 workers, got %d",
			env.app, len(appDeploys))
	}

	t.Logf("multi-worker lock held correctly: 1 PR (#%d) from %d concurrent requests across 2 workers",
		firstPR, n)

	injectApprove(t, firstPR)
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), firstPR)
		return d == nil
	}) {
		t.Fatal("timed out waiting for approved deploy to complete")
	}
}

// TestMultiWorker_NoDoubleDelivery enqueues a single deploy event with two
// workers running and verifies it is processed exactly once — no duplicate PRs.
func TestMultiWorker_NoDoubleDelivery(t *testing.T) {
	startExtraWorker(t, "delivery-worker-2")
	resetAppState(t)
	purgeStaleBranch(t, env.app, env.tag)

	injectDeployRequest(t, "multi-worker: single event delivery test")

	prNumber := waitForPR(t)
	t.Cleanup(func() { cleanupPR(t, prNumber) })

	// Give both workers time to potentially process the same message again.
	time.Sleep(10 * time.Second)

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
		t.Errorf("expected exactly 1 pending deploy with 2 workers, got %d (possible double delivery)",
			len(appDeploys))
	}

	t.Logf("single event delivered exactly once: PR #%d", prNumber)

	injectApprove(t, prNumber)
	if !poll(t, 30*time.Second, func() bool {
		d, _ := env.store.Get(context.Background(), prNumber)
		return d == nil
	}) {
		t.Fatal("timed out waiting for approved deploy to complete")
	}
}
