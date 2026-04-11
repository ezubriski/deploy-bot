package store

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// newTestStore starts an in-process Redis server and returns a Store backed by it.
func newTestStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return New(mr.Addr(), ""), mr
}

func TestAcquireLock_Success(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	acquired, err := s.AcquireLock(ctx, "dev", "myapp", "U123ABC", 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acquired {
		t.Fatal("expected lock to be acquired on empty store")
	}
}

func TestAcquireLock_AlreadyHeld(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// First requester acquires the lock.
	acquired, err := s.AcquireLock(ctx, "dev", "myapp", "U111", 5*time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first acquire failed: acquired=%v err=%v", acquired, err)
	}

	// Second requester must not be able to acquire.
	acquired, err = s.AcquireLock(ctx, "dev", "myapp", "U222", 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error on second acquire: %v", err)
	}
	if acquired {
		t.Fatal("expected second acquire to fail while lock is held")
	}
}

func TestAcquireLock_DifferentAppsIndependent(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	acquired1, _ := s.AcquireLock(ctx, "dev", "app-a", "U111", 5*time.Minute)
	acquired2, _ := s.AcquireLock(ctx, "dev", "app-b", "U222", 5*time.Minute)

	if !acquired1 || !acquired2 {
		t.Fatalf("both apps should be lockable independently: app-a=%v app-b=%v", acquired1, acquired2)
	}
}

func TestReleaseLock_UnblocksSecondAcquire(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, _ = s.AcquireLock(ctx, "dev", "myapp", "U111", 5*time.Minute)

	if err := s.ReleaseLock(ctx, "dev", "myapp"); err != nil {
		t.Fatalf("release lock: %v", err)
	}

	acquired, err := s.AcquireLock(ctx, "dev", "myapp", "U222", 5*time.Minute)
	if err != nil || !acquired {
		t.Fatalf("expected acquire to succeed after release: acquired=%v err=%v", acquired, err)
	}
}

func TestReleaseLock_NoopWhenNotHeld(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Releasing a lock that was never acquired should not error.
	if err := s.ReleaseLock(ctx, "dev", "myapp"); err != nil {
		t.Fatalf("unexpected error releasing non-existent lock: %v", err)
	}
}

func TestAcquireLock_ExpiresAfterTTL(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()

	_, _ = s.AcquireLock(ctx, "dev", "myapp", "U111", 1*time.Second)

	// Fast-forward miniredis time past the TTL.
	mr.FastForward(2 * time.Second)

	acquired, err := s.AcquireLock(ctx, "dev", "myapp", "U222", 5*time.Minute)
	if err != nil || !acquired {
		t.Fatalf("expected acquire to succeed after TTL expiry: acquired=%v err=%v", acquired, err)
	}
}

func TestGetByApp_Found(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	d := &PendingDeploy{
		App:         "myapp",
		Environment: "dev",
		Tag:         "v1.0.0",
		PRNumber:    42,
		PRURL:       "https://github.com/org/repo/pull/42",
		State:       StatePending,
	}
	if err := s.Set(ctx, d, time.Hour); err != nil {
		t.Fatalf("set deploy: %v", err)
	}

	got, err := s.GetByEnvApp(ctx, "dev", "myapp")
	if err != nil {
		t.Fatalf("get by app: %v", err)
	}
	if got == nil {
		t.Fatal("expected a result, got nil")
	}
	if got.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", got.PRNumber)
	}
}

func TestGetByApp_NotFound(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	got, err := s.GetByEnvApp(ctx, "dev", "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestPushHistory_OrderAndTrim(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)

	// Push HistoryMaxLen+2 entries; only the last HistoryMaxLen should survive.
	for i := 0; i < HistoryMaxLen+2; i++ {
		e := HistoryEntry{
			EventType:   "approved",
			App:         "myapp",
			Tag:         "v1.0." + string(rune('0'+i%10)),
			PRNumber:    i + 1,
			RequesterID: "U123",
			CompletedAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := s.PushHistory(ctx, e); err != nil {
			t.Fatalf("push entry %d: %v", i, err)
		}
	}

	entries, err := s.GetHistory(ctx, HistoryMaxLen+10)
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	if len(entries) != HistoryMaxLen {
		t.Fatalf("expected %d entries after trim, got %d", HistoryMaxLen, len(entries))
	}

	// LPUSH means newest (highest index) is first.
	if entries[0].PRNumber != HistoryMaxLen+2 {
		t.Errorf("first entry PRNumber = %d, want %d", entries[0].PRNumber, HistoryMaxLen+2)
	}
}

func TestGetHistory_Limit(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		_ = s.PushHistory(ctx, HistoryEntry{
			EventType:   "rejected",
			App:         "app",
			PRNumber:    i + 1,
			CompletedAt: time.Now(),
		})
	}

	entries, err := s.GetHistory(ctx, 5)
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries with limit=5, got %d", len(entries))
	}
}

func TestGetHistory_Empty(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	entries, err := s.GetHistory(ctx, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty history, got %d entries", len(entries))
	}
}

func TestTryLock_AcquireAndBlock(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	acquired, err := s.TryLock(ctx, "sweeper", 5*time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first TryLock failed: acquired=%v err=%v", acquired, err)
	}

	acquired, err = s.TryLock(ctx, "sweeper", 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error on second TryLock: %v", err)
	}
	if acquired {
		t.Fatal("expected second TryLock to fail while lock is held")
	}
}

func TestTryLock_ExpiresAfterTTL(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()

	_, _ = s.TryLock(ctx, "sweeper", 1*time.Second)
	mr.FastForward(2 * time.Second)

	acquired, err := s.TryLock(ctx, "sweeper", 5*time.Minute)
	if err != nil || !acquired {
		t.Fatalf("expected TryLock to succeed after TTL expiry: acquired=%v err=%v", acquired, err)
	}
}

func TestPRNumberFromKey(t *testing.T) {
	cases := []struct {
		key    string
		want   int
		wantOK bool
	}{
		{"pending:42", 42, true},
		{"pending:1", 1, true},
		{"pending:99999", 99999, true},
		{"lock:myapp", 0, false},
		{"pending:", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := PRNumberFromKey(tc.key)
		if ok != tc.wantOK {
			t.Errorf("PRNumberFromKey(%q): ok=%v, want %v", tc.key, ok, tc.wantOK)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("PRNumberFromKey(%q): number=%d, want %d", tc.key, got, tc.want)
		}
	}
}

// TestPendingDeploy_RoundTripPreservesSlackHandle verifies that the
// SlackChannel and SlackMessageTS fields survive a Set/Get round trip
// through Redis. These fields are populated after the approval post
// succeeds, and downstream features (ArgoCD notification correlation)
// depend on them being preserved across reads.
func TestPendingDeploy_RoundTripPreservesSlackHandle(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	d := &PendingDeploy{
		App:            "myapp",
		Environment:    "prod",
		Tag:            "v1.0.0",
		PRNumber:       77,
		PRURL:          "https://github.com/org/repo/pull/77",
		State:          StatePending,
		ExpiresAt:      time.Now().Add(time.Hour),
		SlackChannel:   "C_DEPLOY",
		SlackMessageTS: "1700000000.123456",
	}
	if err := s.Set(ctx, d, time.Hour); err != nil {
		t.Fatalf("set deploy: %v", err)
	}

	got, err := s.Get(ctx, 77)
	if err != nil {
		t.Fatalf("get deploy: %v", err)
	}
	if got == nil {
		t.Fatal("expected deploy, got nil")
	}
	if got.SlackChannel != "C_DEPLOY" {
		t.Errorf("SlackChannel = %q, want %q", got.SlackChannel, "C_DEPLOY")
	}
	if got.SlackMessageTS != "1700000000.123456" {
		t.Errorf("SlackMessageTS = %q, want %q", got.SlackMessageTS, "1700000000.123456")
	}
}

// TestPendingDeploy_OmitemptyAllowsLegacyDecode verifies that records written
// before the enrichment fields existed (or written by paths that haven't
// populated them yet) decode correctly with empty defaults. This protects
// against in-flight Redis records breaking on bot restart after upgrade.
func TestPendingDeploy_OmitemptyAllowsLegacyDecode(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// A minimal record matching the pre-enrichment shape.
	d := &PendingDeploy{
		App:         "myapp",
		Environment: "dev",
		Tag:         "v0.1",
		PRNumber:    1,
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	if err := s.Set(ctx, d, time.Hour); err != nil {
		t.Fatalf("set deploy: %v", err)
	}
	got, err := s.Get(ctx, 1)
	if err != nil || got == nil {
		t.Fatalf("get deploy: got=%v err=%v", got, err)
	}
	if got.SlackChannel != "" || got.SlackMessageTS != "" {
		t.Errorf("expected empty enrichment fields on legacy decode, got ch=%q ts=%q",
			got.SlackChannel, got.SlackMessageTS)
	}
}

// TestSetSlackHandle_PreservesOtherFieldsAndState is the key race-safety
// test: a concurrent writer transitions state to "merging" between our
// Get and Set, and SetSlackHandle must pick up the new state rather than
// clobber it back to "pending". This is the scenario described in the
// SetSlackHandle doc comment: a fast-clicking approver triggers the
// state transition before the write-back completes.
func TestSetSlackHandle_PreservesOtherFieldsAndState(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	d := &PendingDeploy{
		App:         "myapp",
		Environment: "prod",
		Tag:         "v1.0.0",
		PRNumber:    42,
		PRURL:       "https://github.com/org/repo/pull/42",
		Requester:   "gh-user",
		State:       StatePending,
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	if err := s.Set(ctx, d, time.Hour); err != nil {
		t.Fatalf("set deploy: %v", err)
	}

	// Simulate a concurrent approval transition before our write-back
	// runs. In production this is the handleApprove path calling
	// UpdateState(StateMerging).
	if err := s.UpdateState(ctx, 42, StateMerging); err != nil {
		t.Fatalf("concurrent state update: %v", err)
	}

	if err := s.SetSlackHandle(ctx, 42, "C_DEPLOY", "1700000000.999999"); err != nil {
		t.Fatalf("set slack handle: %v", err)
	}

	got, err := s.Get(ctx, 42)
	if err != nil || got == nil {
		t.Fatalf("get deploy: got=%v err=%v", got, err)
	}
	if got.State != StateMerging {
		t.Errorf("state = %q, want %q — SetSlackHandle clobbered concurrent UpdateState",
			got.State, StateMerging)
	}
	if got.SlackChannel != "C_DEPLOY" || got.SlackMessageTS != "1700000000.999999" {
		t.Errorf("slack handle = (%q, %q), want (C_DEPLOY, 1700000000.999999)",
			got.SlackChannel, got.SlackMessageTS)
	}
	if got.App != "myapp" || got.Requester != "gh-user" {
		t.Errorf("other fields clobbered: app=%q requester=%q", got.App, got.Requester)
	}
}

// TestSetSlackHandle_NoOpWhenRecordGone verifies that SetSlackHandle returns
// nil (not an error) when the pending record has already been deleted —
// e.g. because the deploy was approved and the pending record removed
// before the Slack post wrote back. Callers treat a missing handle as
// "not correlatable" rather than a failure; this test pins that contract.
func TestSetSlackHandle_NoOpWhenRecordGone(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if err := s.SetSlackHandle(ctx, 999, "C_DEPLOY", "1700000000.000000"); err != nil {
		t.Errorf("expected nil error for missing record, got %v", err)
	}
}

// TestHistoryEntry_RoundTripPreservesEnrichmentFields verifies the same
// fields survive a PushHistory/GetHistory round trip on the history list.
// The bot populates these on the approved/merged history push so the
// ArgoCD handler can correlate a synced revision back to the deploy that
// produced it after the entry has aged out of the pending set.
func TestHistoryEntry_RoundTripPreservesEnrichmentFields(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	e := HistoryEntry{
		EventType:       "approved",
		App:             "myapp",
		Environment:     "prod",
		Tag:             "v1.2.3",
		PRNumber:        99,
		PRURL:           "https://github.com/org/repo/pull/99",
		RequesterID:     "U123",
		CompletedAt:     time.Now().UTC().Truncate(time.Second),
		GitopsCommitSHA: "deadbeef00000000",
		SlackChannel:    "C_DEPLOY",
		SlackMessageTS:  "1700000123.456789",
	}
	if err := s.PushHistory(ctx, e); err != nil {
		t.Fatalf("push history: %v", err)
	}

	entries, err := s.GetHistory(ctx, 10)
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	got := entries[0]
	if got.GitopsCommitSHA != "deadbeef00000000" {
		t.Errorf("GitopsCommitSHA = %q, want %q", got.GitopsCommitSHA, "deadbeef00000000")
	}
	if got.SlackChannel != "C_DEPLOY" {
		t.Errorf("SlackChannel = %q, want %q", got.SlackChannel, "C_DEPLOY")
	}
	if got.SlackMessageTS != "1700000123.456789" {
		t.Errorf("SlackMessageTS = %q, want %q", got.SlackMessageTS, "1700000123.456789")
	}
}

// TestFindHistoryBySHA_Found verifies the ArgoCD-correlation lookup path:
// push a few history entries with distinct GitopsCommitSHA values and
// assert the correct one comes back.
func TestFindHistoryBySHA_Found(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)
	for i, sha := range []string{"sha-oldest", "sha-middle", "sha-newest"} {
		if err := s.PushHistory(ctx, HistoryEntry{
			EventType:       "approved",
			App:             "myapp",
			Environment:     "prod",
			Tag:             "v1.0." + string(rune('0'+i)),
			PRNumber:        i + 1,
			RequesterID:     "U1",
			CompletedAt:     base.Add(time.Duration(i) * time.Second),
			GitopsCommitSHA: sha,
		}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}

	got, err := s.FindHistoryBySHA(ctx, "sha-middle")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil {
		t.Fatal("expected match, got nil")
	}
	if got.GitopsCommitSHA != "sha-middle" {
		t.Errorf("sha = %q, want sha-middle", got.GitopsCommitSHA)
	}
	if got.PRNumber != 2 {
		t.Errorf("pr = %d, want 2", got.PRNumber)
	}
}

// TestFindHistoryBySHA_NotFound verifies the unmatched case returns
// (nil, nil) — no error, no entry. The bot treats this as "this deploy
// was not made by deploy-bot" and drops the notification.
func TestFindHistoryBySHA_NotFound(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_ = s.PushHistory(ctx, HistoryEntry{
		App:             "myapp",
		Tag:             "v1",
		CompletedAt:     time.Now(),
		GitopsCommitSHA: "sha-a",
	})

	got, err := s.FindHistoryBySHA(ctx, "sha-does-not-exist")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// TestFindHistoryBySHA_EmptySHAIsNoMatch verifies that passing an empty
// sha short-circuits to (nil, nil) without scanning — the bot passes
// incoming payload fields directly, and an empty sha means "ArgoCD did
// not carry a revision," which is not an error.
func TestFindHistoryBySHA_EmptySHAIsNoMatch(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Seed an entry that also has an empty sha — the lookup must NOT
	// match against it, because "empty matches empty" would silently
	// pair every no-sha notification with the most recent legacy entry.
	_ = s.PushHistory(ctx, HistoryEntry{
		App:         "legacy",
		CompletedAt: time.Now(),
	})

	got, err := s.FindHistoryBySHA(ctx, "")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got != nil {
		t.Errorf("empty sha must not match, got %+v", got)
	}
}

func TestPushHistory_AppFilter(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_ = s.PushHistory(ctx, HistoryEntry{EventType: "approved", App: "app-a", PRNumber: 1, CompletedAt: time.Now()})
	_ = s.PushHistory(ctx, HistoryEntry{EventType: "approved", App: "app-b", PRNumber: 2, CompletedAt: time.Now()})
	_ = s.PushHistory(ctx, HistoryEntry{EventType: "rejected", App: "app-a", PRNumber: 3, CompletedAt: time.Now()})

	all, err := s.GetHistory(ctx, 10)
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 total entries, got %d", len(all))
	}

	// Filter app-a entries (done at the command layer, verify raw store is unfiltered).
	var appA []HistoryEntry
	for _, e := range all {
		if e.App == "app-a" {
			appA = append(appA, e)
		}
	}
	if len(appA) != 2 {
		t.Fatalf("expected 2 app-a entries, got %d", len(appA))
	}
}
