// Tests in this file exercise the Postgres-backed store methods
// (pending_deploys + history) via an ephemeral testcontainer.
//
// Lives in `package store_test` rather than `package store` so it
// can import internal/storetest without an import cycle (storetest
// imports store). Tests here skip gracefully when no Docker/Podman
// runtime is available, via storetest.NewStoreWithRedis's internal
// t.Skip — see internal/storetest for the gating behaviour.
//
// Redis-only tests (locks, TryLock, thread ts) stay in
// store_test.go, in the internal `package store`, where they
// continue to use miniredis with no Postgres dependency.
package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ezubriski/deploy-bot/internal/store"
	"github.com/ezubriski/deploy-bot/internal/storetest"
)

func TestGetByApp_Found(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	d := &store.PendingDeploy{
		GitHubOrg:   "org",
		GitHubRepo:  "repo",
		App:         "myapp",
		Environment: "dev",
		Tag:         "v1.0.0",
		PRNumber:    42,
		PRURL:       "https://github.com/org/repo/pull/42",
		Requester:   "me",
		RequesterID: "U1",
		State:       store.StatePending,
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
	s := storetest.NewStore(t)
	ctx := context.Background()

	got, err := s.GetByEnvApp(ctx, "dev", "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// TestPushHistory_OrderingNewestFirst verifies that GetHistory
// returns rows newest-first (ORDER BY completed_at DESC). The 1.x
// LPUSH+LTRIM 100-entry-cap test is gone because Postgres has no
// such cap — retention is governed by the retention ticker per the
// configured history_retention window.
func TestPushHistory_OrderingNewestFirst(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)

	for i := 0; i < 10; i++ {
		e := store.HistoryEntry{
			EventType:   "approved",
			App:         "myapp",
			Environment: "dev",
			Tag:         "v1.0." + string(rune('0'+i%10)),
			PRNumber:    i + 1,
			RequesterID: "U123",
			CompletedAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := s.PushHistory(ctx, e); err != nil {
			t.Fatalf("push entry %d: %v", i, err)
		}
	}

	entries, err := s.GetHistory(ctx, "", 20)
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	if len(entries) != 10 {
		t.Fatalf("expected 10 entries, got %d", len(entries))
	}
	// Newest first: entries[0] is the last pushed (i=9, pr=10).
	if entries[0].PRNumber != 10 {
		t.Errorf("first entry PRNumber = %d, want 10", entries[0].PRNumber)
	}
	if entries[9].PRNumber != 1 {
		t.Errorf("last entry PRNumber = %d, want 1", entries[9].PRNumber)
	}
}

func TestGetHistory_Limit(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		_ = s.PushHistory(ctx, store.HistoryEntry{
			EventType:   "rejected",
			App:         "app",
			Environment: "dev",
			Tag:         "v1",
			PRNumber:    i + 1,
			RequesterID: "U1",
			CompletedAt: time.Now(),
		})
	}

	entries, err := s.GetHistory(ctx, "", 5)
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries with limit=5, got %d", len(entries))
	}
}

func TestGetHistory_Empty(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	entries, err := s.GetHistory(ctx, "", 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty history, got %d entries", len(entries))
	}
}

// TestPendingDeploy_RoundTripPreservesSlackHandle verifies that the
// SlackChannel and SlackMessageTS fields survive a Set/Get round
// trip. These fields are populated after the approval post succeeds,
// and downstream features (ArgoCD notification correlation) depend
// on them being preserved across reads.
func TestPendingDeploy_RoundTripPreservesSlackHandle(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	d := &store.PendingDeploy{
		GitHubOrg:      "org",
		GitHubRepo:     "repo",
		App:            "myapp",
		Environment:    "prod",
		Tag:            "v1.0.0",
		PRNumber:       77,
		PRURL:          "https://github.com/org/repo/pull/77",
		Requester:      "me",
		RequesterID:    "U1",
		State:          store.StatePending,
		ExpiresAt:      time.Now().Add(time.Hour),
		SlackChannel:   "C_DEPLOY",
		SlackMessageTS: "1700000000.123456",
	}
	if err := s.Set(ctx, d, time.Hour); err != nil {
		t.Fatalf("set deploy: %v", err)
	}

	got, err := s.Get(ctx, "org", "repo", 77)
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

// TestSetSlackHandle_PreservesOtherFieldsAndState is the key race-
// safety test: a concurrent writer transitions state to "merging"
// between our Get and write-back, and SetSlackHandle must not
// clobber the new state. With Postgres, this is handled by a single
// UPDATE statement that only touches slack_channel/slack_message_ts
// — MVCC guarantees the concurrent UpdateState cannot lose.
func TestSetSlackHandle_PreservesOtherFieldsAndState(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	d := &store.PendingDeploy{
		GitHubOrg:   "org",
		GitHubRepo:  "repo",
		App:         "myapp",
		Environment: "prod",
		Tag:         "v1.0.0",
		PRNumber:    42,
		PRURL:       "https://github.com/org/repo/pull/42",
		Requester:   "gh-user",
		RequesterID: "U1",
		State:       store.StatePending,
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	if err := s.Set(ctx, d, time.Hour); err != nil {
		t.Fatalf("set deploy: %v", err)
	}

	// Simulate a concurrent approval transition before our write-back.
	if err := s.UpdateState(ctx, "org", "repo", 42, store.StateMerging); err != nil {
		t.Fatalf("concurrent state update: %v", err)
	}

	if err := s.SetSlackHandle(ctx, "org", "repo", 42, "C_DEPLOY", "1700000000.999999"); err != nil {
		t.Fatalf("set slack handle: %v", err)
	}

	got, err := s.Get(ctx, "org", "repo", 42)
	if err != nil || got == nil {
		t.Fatalf("get deploy: got=%v err=%v", got, err)
	}
	if got.State != store.StateMerging {
		t.Errorf("state = %q, want %q — SetSlackHandle clobbered concurrent UpdateState",
			got.State, store.StateMerging)
	}
	if got.SlackChannel != "C_DEPLOY" || got.SlackMessageTS != "1700000000.999999" {
		t.Errorf("slack handle = (%q, %q), want (C_DEPLOY, 1700000000.999999)",
			got.SlackChannel, got.SlackMessageTS)
	}
	if got.App != "myapp" || got.Requester != "gh-user" {
		t.Errorf("other fields clobbered: app=%q requester=%q", got.App, got.Requester)
	}
}

// TestSetSlackHandle_NoOpWhenRecordGone verifies that SetSlackHandle
// returns nil (not an error) when no row exists. The Postgres-backed
// UPDATE is a no-op on zero rows affected; callers treat a missing
// handle as "not correlatable" rather than a failure.
func TestSetSlackHandle_NoOpWhenRecordGone(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()
	if err := s.SetSlackHandle(ctx, "org", "repo", 999, "C_DEPLOY", "1700000000.000000"); err != nil {
		t.Errorf("expected nil error for missing record, got %v", err)
	}
}

// TestUpdateState_NotFoundReturnsErr is new for 2.0: the Postgres-
// backed UpdateState surfaces ErrPendingNotFound when the row
// doesn't exist, so callers can distinguish "nothing to update"
// from infrastructure failure. 1.x returned a generic "deploy N
// not found" error instead.
func TestUpdateState_NotFoundReturnsErr(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	err := s.UpdateState(ctx, "org", "repo", 12345, store.StateMerging)
	if err == nil {
		t.Fatal("expected error for missing pending row")
	}
	if !strings.Contains(err.Error(), "pending deploy not found") {
		t.Errorf("expected ErrPendingNotFound wrap, got %v", err)
	}
}

// TestHistoryEntry_RoundTripPreservesEnrichmentFields verifies that
// the same fields survive a PushHistory/GetHistory round trip. The
// bot populates these on the approved/merged history push so the
// ArgoCD handler can correlate a synced revision back to the deploy
// that produced it.
func TestHistoryEntry_RoundTripPreservesEnrichmentFields(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	e := store.HistoryEntry{
		GitHubOrg:       "org",
		GitHubRepo:      "repo",
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

	entries, err := s.GetHistory(ctx, "", 10)
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	got := entries[0]
	if got.GitHubOrg != "org" || got.GitHubRepo != "repo" {
		t.Errorf("github_org/repo = %q/%q, want org/repo", got.GitHubOrg, got.GitHubRepo)
	}
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

// TestFindHistoryBySHA_Found verifies the ArgoCD-correlation lookup
// path: push a few history entries with distinct GitopsCommitSHA
// values and assert the correct one comes back. Backed by the
// history_gitops_sha_idx partial index.
func TestFindHistoryBySHA_Found(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)
	for i, sha := range []string{"sha-oldest", "sha-middle", "sha-newest"} {
		if err := s.PushHistory(ctx, store.HistoryEntry{
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

func TestFindHistoryBySHA_NotFound(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	_ = s.PushHistory(ctx, store.HistoryEntry{
		EventType:       "approved",
		App:             "myapp",
		Environment:     "dev",
		Tag:             "v1",
		RequesterID:     "U1",
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

// TestFindHistoryBySHA_EmptySHAIsNoMatch verifies that passing an
// empty sha short-circuits to (nil, nil) without hitting the
// database. Guards against the silent-coincidental-match failure
// mode where an empty payload SHA would match a legacy entry that
// also has an empty SHA.
func TestFindHistoryBySHA_EmptySHAIsNoMatch(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	_ = s.PushHistory(ctx, store.HistoryEntry{
		EventType:   "approved",
		App:         "legacy",
		Environment: "dev",
		Tag:         "v1",
		RequesterID: "U1",
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

// TestPushHistory_AppFilter verifies that the raw store returns every
// entry unfiltered — app-level filtering happens at the command layer
// via a client-side filter on GetHistory's result. Postgres could do
// this via WHERE if the command layer pushed the filter down, but
// that's a later optimization.
func TestPushHistory_AppFilter(t *testing.T) {
	s := storetest.NewStore(t)
	ctx := context.Background()

	_ = s.PushHistory(ctx, store.HistoryEntry{
		EventType: "approved", App: "app-a", Environment: "dev", Tag: "v1",
		PRNumber: 1, RequesterID: "U1", CompletedAt: time.Now(),
	})
	_ = s.PushHistory(ctx, store.HistoryEntry{
		EventType: "approved", App: "app-b", Environment: "dev", Tag: "v1",
		PRNumber: 2, RequesterID: "U1", CompletedAt: time.Now(),
	})
	_ = s.PushHistory(ctx, store.HistoryEntry{
		EventType: "rejected", App: "app-a", Environment: "dev", Tag: "v1",
		PRNumber: 3, RequesterID: "U1", CompletedAt: time.Now(),
	})

	all, err := s.GetHistory(ctx, "", 10)
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 total entries, got %d", len(all))
	}

	var appA []store.HistoryEntry
	for _, e := range all {
		if e.App == "app-a" {
			appA = append(appA, e)
		}
	}
	if len(appA) != 2 {
		t.Fatalf("expected 2 app-a entries, got %d", len(appA))
	}
}
