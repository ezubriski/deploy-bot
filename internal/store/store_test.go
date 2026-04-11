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

// TestPendingDeploy_RoundTripPreservesEnrichmentFields verifies that the
// GitopsCommitSHA, SlackChannel, and SlackMessageTS fields survive a Set/Get
// round trip through Redis. These fields are populated incrementally over
// the deploy lifecycle (slack handle at request time, sha at merge time)
// and downstream features (ArgoCD notification correlation) depend on them
// being preserved across reads.
func TestPendingDeploy_RoundTripPreservesEnrichmentFields(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	d := &PendingDeploy{
		App:             "myapp",
		Environment:     "prod",
		Tag:             "v1.0.0",
		PRNumber:        77,
		PRURL:           "https://github.com/org/repo/pull/77",
		State:           StatePending,
		ExpiresAt:       time.Now().Add(time.Hour),
		GitopsCommitSHA: "abcdef1234567890",
		SlackChannel:    "C_DEPLOY",
		SlackMessageTS:  "1700000000.123456",
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
	if got.GitopsCommitSHA != "abcdef1234567890" {
		t.Errorf("GitopsCommitSHA = %q, want %q", got.GitopsCommitSHA, "abcdef1234567890")
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
	if got.GitopsCommitSHA != "" || got.SlackChannel != "" || got.SlackMessageTS != "" {
		t.Errorf("expected empty enrichment fields on legacy decode, got sha=%q ch=%q ts=%q",
			got.GitopsCommitSHA, got.SlackChannel, got.SlackMessageTS)
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
