package store

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// newTestStore starts an in-process Redis server and returns a Store
// backed by it. The Postgres pool is nil — tests that want to
// exercise history/pending methods must use storetest.NewStore (in
// store_pg_test.go, package store_test) to get a testcontainer-backed
// pool. This helper is only for Redis-backed lock/thread tests.
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

// TestGetByApp_*, TestPushHistory_*, TestGetHistory_*,
// TestFindHistoryBySHA_*, TestPendingDeploy_*, TestSetSlackHandle_*,
// TestHistoryEntry_*, and TestUpdateState_* moved to store_pg_test.go
// (package store_test) where they can import internal/storetest to
// obtain a testcontainer-backed Postgres. Only Redis-backed locks
// and thread TS tests stay here.

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
