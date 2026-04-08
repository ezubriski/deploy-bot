package reposcanner

import (
	"context"
	"sync"
	"testing"
	"time"

	slackPkg "github.com/slack-go/slack"
)

// captureSlack records channels and messages.
type captureSlack struct {
	mu       sync.Mutex
	messages []capturedMsg
}

type capturedMsg struct {
	Channel string
}

func (c *captureSlack) PostMessageContext(_ context.Context, channelID string, _ ...slackPkg.MsgOption) (string, string, error) {
	c.mu.Lock()
	c.messages = append(c.messages, capturedMsg{Channel: channelID})
	c.mu.Unlock()
	return "", "", nil
}
func (c *captureSlack) PostEphemeralContext(_ context.Context, _, _ string, _ ...slackPkg.MsgOption) (string, error) {
	return "", nil
}
func (c *captureSlack) UpdateMessageContext(_ context.Context, _, _ string, _ ...slackPkg.MsgOption) (string, string, string, error) {
	return "", "", "", nil
}
func (c *captureSlack) OpenViewContext(_ context.Context, _ string, _ slackPkg.ModalViewRequest) (*slackPkg.ViewResponse, error) {
	return nil, nil
}

func newTestTracker() *conflictTracker {
	ct := newConflictTracker(nil)
	// Use a fixed clock for deterministic tests.
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ct.nowFunc = func() time.Time { return now }
	return ct
}

func advanceClock(ct *conflictTracker, d time.Duration) {
	prev := ct.nowFunc()
	ct.nowFunc = func() time.Time { return prev.Add(d) }
}

func TestConflictTracker_BatchesSingleMessage(t *testing.T) {
	ct := newTestTracker()
	sl := &captureSlack{}
	conflicts := map[string]conflictInfo{
		"a\x00dev":  {App: "a", Env: "dev", SourceRepo: "org/a"},
		"b\x00prod": {App: "b", Env: "prod", SourceRepo: "org/b"},
	}

	ct.emitWarnings(context.Background(), sl, "C_DEPLOY", ".deploy-bot.json", conflicts)

	sl.mu.Lock()
	defer sl.mu.Unlock()
	if len(sl.messages) != 1 {
		t.Fatalf("expected 1 batched message for 2 conflicts, got %d", len(sl.messages))
	}
}

func TestConflictTracker_Debounce(t *testing.T) {
	ct := newTestTracker()
	sl := &captureSlack{}
	conflicts := map[string]conflictInfo{
		"myapp\x00dev": {App: "myapp", Env: "dev", SourceRepo: "org/myapp"},
	}

	ct.emitWarnings(context.Background(), sl, "C_DEPLOY", ".deploy-bot.json", conflicts)
	// Same conflict again — already warned, no new conflicts to report.
	advanceClock(ct, 25*time.Minute)
	ct.emitWarnings(context.Background(), sl, "C_DEPLOY", ".deploy-bot.json", conflicts)

	sl.mu.Lock()
	defer sl.mu.Unlock()
	if len(sl.messages) != 1 {
		t.Fatalf("expected 1 message (debounced), got %d", len(sl.messages))
	}
}

func TestConflictTracker_CooldownBlocksReintroduced(t *testing.T) {
	ct := newTestTracker()
	sl := &captureSlack{}
	conflicts := map[string]conflictInfo{
		"myapp\x00dev": {App: "myapp", Env: "dev", SourceRepo: "org/myapp"},
	}

	// First warning.
	ct.emitWarnings(context.Background(), sl, "C_DEPLOY", ".deploy-bot.json", conflicts)
	// Resolve.
	advanceClock(ct, 5*time.Minute)
	ct.emitWarnings(context.Background(), sl, "C_DEPLOY", ".deploy-bot.json", map[string]conflictInfo{})
	// Reintroduce within cooldown — should be blocked.
	advanceClock(ct, 5*time.Minute) // 10 min total < 20 min cooldown
	ct.emitWarnings(context.Background(), sl, "C_DEPLOY", ".deploy-bot.json", conflicts)

	sl.mu.Lock()
	defer sl.mu.Unlock()
	if len(sl.messages) != 1 {
		t.Fatalf("expected 1 message (cooldown blocks reintroduced), got %d", len(sl.messages))
	}
}

func TestConflictTracker_ReintroducedAfterCooldown(t *testing.T) {
	ct := newTestTracker()
	sl := &captureSlack{}
	conflicts := map[string]conflictInfo{
		"myapp\x00dev": {App: "myapp", Env: "dev", SourceRepo: "org/myapp"},
	}

	// First warning.
	ct.emitWarnings(context.Background(), sl, "C_DEPLOY", ".deploy-bot.json", conflicts)
	// Resolve.
	advanceClock(ct, 10*time.Minute)
	ct.emitWarnings(context.Background(), sl, "C_DEPLOY", ".deploy-bot.json", map[string]conflictInfo{})
	// Reintroduce after cooldown.
	advanceClock(ct, 15*time.Minute) // 25 min total > 20 min cooldown
	ct.emitWarnings(context.Background(), sl, "C_DEPLOY", ".deploy-bot.json", conflicts)

	sl.mu.Lock()
	defer sl.mu.Unlock()
	if len(sl.messages) != 2 {
		t.Fatalf("expected 2 messages (initial + after cooldown), got %d", len(sl.messages))
	}
}

func TestConflictTracker_EmptyChannel(t *testing.T) {
	ct := newTestTracker()
	sl := &captureSlack{}
	conflicts := map[string]conflictInfo{
		"myapp\x00dev": {App: "myapp", Env: "dev", SourceRepo: "org/myapp"},
	}

	ct.emitWarnings(context.Background(), sl, "", ".deploy-bot.json", conflicts)

	sl.mu.Lock()
	defer sl.mu.Unlock()
	if len(sl.messages) != 0 {
		t.Fatalf("expected no warnings with empty channel, got %d", len(sl.messages))
	}
}

func TestConflictTracker_NewConflictAddedWithinCooldown(t *testing.T) {
	ct := newTestTracker()
	sl := &captureSlack{}

	// First conflict.
	ct.emitWarnings(context.Background(), sl, "C_DEPLOY", ".deploy-bot.json", map[string]conflictInfo{
		"a\x00dev": {App: "a", Env: "dev", SourceRepo: "org/a"},
	})

	// New conflict added within cooldown — should be blocked.
	advanceClock(ct, 10*time.Minute)
	ct.emitWarnings(context.Background(), sl, "C_DEPLOY", ".deploy-bot.json", map[string]conflictInfo{
		"a\x00dev": {App: "a", Env: "dev", SourceRepo: "org/a"},
		"b\x00dev": {App: "b", Env: "dev", SourceRepo: "org/b"},
	})

	sl.mu.Lock()
	count := len(sl.messages)
	sl.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected 1 message (cooldown blocks new conflict), got %d", count)
	}

	// After cooldown, the new conflict should be posted.
	advanceClock(ct, 15*time.Minute) // 25 min total
	ct.emitWarnings(context.Background(), sl, "C_DEPLOY", ".deploy-bot.json", map[string]conflictInfo{
		"a\x00dev": {App: "a", Env: "dev", SourceRepo: "org/a"},
		"b\x00dev": {App: "b", Env: "dev", SourceRepo: "org/b"},
	})

	sl.mu.Lock()
	defer sl.mu.Unlock()
	if len(sl.messages) != 2 {
		t.Fatalf("expected 2 messages (initial + after cooldown), got %d", len(sl.messages))
	}
}
