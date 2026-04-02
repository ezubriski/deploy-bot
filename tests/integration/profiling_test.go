//go:build integration

package integration

// TestProfiling runs a realistic mix of deploy operations to warm up the bot
// and provide a meaningful sample for CPU/heap profiling.
//
// A CPU profile is written to cpu.prof in the working directory on completion.
// Analyse it with:
//
//	go tool pprof cpu.prof
//
// While this test runs you can also capture profiles from the deployed pod:
//
//	kubectl port-forward pod/<deploy-bot-pod> 9090:9090
//	go tool pprof http://localhost:9090/debug/pprof/heap
//	go tool pprof http://localhost:9090/debug/pprof/profile   # 30s CPU sample
//	kubectl top pod -l app=deploy-bot -w                      # live RSS/CPU

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/ezubriski/deploy-bot/internal/bot"
	"github.com/ezubriski/deploy-bot/internal/queue"
)

// profilingApps assigns a dedicated app to each cycle so they run without lock
// contention and produce distinct branch names even when sharing a tag.
var profilingApps = []string{
	"nginx-01", // approve cycle 1
	"nginx-02", // approve cycle 2
	"nginx-03", // approve cycle 3
	"nginx-04", // reject cycle
	"nginx-05", // cancel cycle
}

func TestProfiling(t *testing.T) {
	cpuFile, err := os.Create("cpu.prof")
	if err != nil {
		t.Fatalf("create cpu.prof: %v", err)
	}
	defer cpuFile.Close()

	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		t.Fatalf("start CPU profile: %v", err)
	}
	defer func() {
		pprof.StopCPUProfile()
		t.Logf("CPU profile written to cpu.prof")
	}()

	// Approve cycles — the dominant production path.
	for i, app := range profilingApps[:3] {
		t.Logf("approve cycle %d (app: %s)", i+1, app)

		tag := pickTagFor(t, app)
		resetAppStateFor(t, app)
		purgeStaleBranch(t, app, tag)
		injectDeployRequestFor(t, app, tag, fmt.Sprintf("profiling: approve cycle %d", i+1))

		prNumber := waitForPRFor(t, app, tag)
		prTag := tag
		t.Cleanup(func() { cleanupPRWithTag(t, prNumber, prTag) })

		injectApprove(t, prNumber)
		if !poll(t, 60*time.Second, func() bool {
			d, _ := env.store.Get(context.Background(), prNumber)
			return d == nil
		}) {
			t.Fatalf("approve cycle %d: timed out waiting for merge (PR #%d)", i+1, prNumber)
		}
		t.Logf("approve cycle %d complete (PR #%d, tag %s)", i+1, prNumber, tag)
	}

	// Reject cycle.
	{
		app := profilingApps[3]
		t.Logf("reject cycle (app: %s)", app)

		tag := pickTagFor(t, app)
		resetAppStateFor(t, app)
		purgeStaleBranch(t, app, tag)
		injectDeployRequestFor(t, app, tag, "profiling: reject cycle")

		prNumber := waitForPRFor(t, app, tag)
		prTag := tag
		t.Cleanup(func() { cleanupPRWithTag(t, prNumber, prTag) })

		injectRejectSubmit(t, prNumber, "profiling: rejected")
		if !poll(t, 30*time.Second, func() bool {
			d, _ := env.store.Get(context.Background(), prNumber)
			return d == nil
		}) {
			t.Fatalf("reject cycle: timed out (PR #%d)", prNumber)
		}
		t.Logf("reject cycle complete (PR #%d, tag %s)", prNumber, tag)
	}

	// Cancel cycle.
	{
		app := profilingApps[4]
		t.Logf("cancel cycle (app: %s)", app)

		tag := pickTagFor(t, app)
		resetAppStateFor(t, app)
		purgeStaleBranch(t, app, tag)
		injectDeployRequestFor(t, app, tag, "profiling: cancel cycle")

		prNumber := waitForPRFor(t, app, tag)
		prTag := tag
		t.Cleanup(func() { cleanupPRWithTag(t, prNumber, prTag) })

		injectSlashCommand(t, fmt.Sprintf("cancel %d", prNumber))
		if !poll(t, 30*time.Second, func() bool {
			d, _ := env.store.Get(context.Background(), prNumber)
			return d == nil
		}) {
			t.Fatalf("cancel cycle: timed out (PR #%d)", prNumber)
		}
		t.Logf("cancel cycle complete (PR #%d, tag %s)", prNumber, tag)
	}

	// Read-only paths.
	injectSlashCommand(t, "status")
	injectSlashCommand(t, "history "+profilingApps[0])
	time.Sleep(3 * time.Second)

	// In-process memory snapshot.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("--- memory snapshot ---")
	t.Logf("HeapAlloc:  %6d KB  (live heap objects)", ms.HeapAlloc/1024)
	t.Logf("HeapSys:    %6d KB  (heap memory from OS)", ms.HeapSys/1024)
	t.Logf("HeapInuse:  %6d KB  (spans with at least one object)", ms.HeapInuse/1024)
	t.Logf("StackInuse: %6d KB", ms.StackInuse/1024)
	t.Logf("Sys:        %6d KB  (total memory from OS)", ms.Sys/1024)
	t.Logf("NumGC:      %d", ms.NumGC)
}

// resetAppStateFor is like resetAppState but targets a specific app instead of env.app.
func resetAppStateFor(t *testing.T, app string) {
	t.Helper()
	ctx := context.Background()
	_ = env.store.ReleaseLock(ctx, env.environment, app)
	deploys, err := env.store.GetAll(ctx)
	if err != nil {
		t.Fatalf("resetAppStateFor %s: %v", app, err)
	}
	for _, d := range deploys {
		if d.App == app {
			_ = env.store.Delete(ctx, d.PRNumber)
		}
	}
}

// injectDeployRequestFor enqueues a deploy request for a specific app and tag.
func injectDeployRequestFor(t *testing.T, app, tag, reason string) {
	t.Helper()
	evt := socketmode.Event{
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
	if err := queue.Enqueue(context.Background(), env.store.Redis(), evt); err != nil {
		t.Fatalf("injectDeployRequestFor %s@%s: %v", app, tag, err)
	}
}

// purgeStaleBranch deletes the deploy branch for app+tag if it still exists in
// GitHub from a previous failed run. Errors are ignored — the branch may not exist.
func purgeStaleBranch(t *testing.T, app, tag string) {
	t.Helper()
	branch := deployBranch(env.environment, app, tag)
	if err := env.ghClient.DeleteBranch(context.Background(), branch); err != nil {
		t.Logf("purgeStaleBranch: %s: %v (may not exist)", branch, err)
	}
}

// waitForPRFor polls until a pending deploy for the given app and tag appears in Redis.
func waitForPRFor(t *testing.T, app, tag string) int {
	t.Helper()
	var prNumber int
	if !poll(t, 60*time.Second, func() bool {
		deploys, _ := env.store.GetAll(context.Background())
		for _, d := range deploys {
			if d.App == app && d.Tag == tag {
				prNumber = d.PRNumber
				return true
			}
		}
		return false
	}) {
		t.Fatalf("timed out waiting for deploy PR (app %s, tag %s)", app, tag)
	}
	return prNumber
}
