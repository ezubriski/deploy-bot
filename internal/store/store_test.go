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
	return New(mr.Addr()), mr
}

func TestAcquireLock_Success(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	acquired, err := s.AcquireLock(ctx, "myapp", "U123ABC", 5*time.Minute)
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
	acquired, err := s.AcquireLock(ctx, "myapp", "U111", 5*time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first acquire failed: acquired=%v err=%v", acquired, err)
	}

	// Second requester must not be able to acquire.
	acquired, err = s.AcquireLock(ctx, "myapp", "U222", 5*time.Minute)
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

	acquired1, _ := s.AcquireLock(ctx, "app-a", "U111", 5*time.Minute)
	acquired2, _ := s.AcquireLock(ctx, "app-b", "U222", 5*time.Minute)

	if !acquired1 || !acquired2 {
		t.Fatalf("both apps should be lockable independently: app-a=%v app-b=%v", acquired1, acquired2)
	}
}

func TestReleaseLock_UnblocksSecondAcquire(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, _ = s.AcquireLock(ctx, "myapp", "U111", 5*time.Minute)

	if err := s.ReleaseLock(ctx, "myapp"); err != nil {
		t.Fatalf("release lock: %v", err)
	}

	acquired, err := s.AcquireLock(ctx, "myapp", "U222", 5*time.Minute)
	if err != nil || !acquired {
		t.Fatalf("expected acquire to succeed after release: acquired=%v err=%v", acquired, err)
	}
}

func TestReleaseLock_NoopWhenNotHeld(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Releasing a lock that was never acquired should not error.
	if err := s.ReleaseLock(ctx, "myapp"); err != nil {
		t.Fatalf("unexpected error releasing non-existent lock: %v", err)
	}
}

func TestAcquireLock_ExpiresAfterTTL(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()

	_, _ = s.AcquireLock(ctx, "myapp", "U111", 1*time.Second)

	// Fast-forward miniredis time past the TTL.
	mr.FastForward(2 * time.Second)

	acquired, err := s.AcquireLock(ctx, "myapp", "U222", 5*time.Minute)
	if err != nil || !acquired {
		t.Fatalf("expected acquire to succeed after TTL expiry: acquired=%v err=%v", acquired, err)
	}
}

func TestGetByApp_Found(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	d := &PendingDeploy{
		App:      "myapp",
		Tag:      "v1.0.0",
		PRNumber: 42,
		PRURL:    "https://github.com/org/repo/pull/42",
		State:    StatePending,
	}
	if err := s.Set(ctx, d, time.Hour); err != nil {
		t.Fatalf("set deploy: %v", err)
	}

	got, err := s.GetByApp(ctx, "myapp")
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

	got, err := s.GetByApp(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}
