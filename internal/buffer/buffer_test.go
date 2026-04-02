package buffer

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/queue"
)

func newTestBuffer(t *testing.T, size int) (*Buffer, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return New(size, rdb, zap.NewNop()), rdb
}

func newTestBufferWithMR(t *testing.T, size int) (*Buffer, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return New(size, rdb, zap.NewNop()), rdb, mr
}

func slashCommandEvent() socketmode.Event {
	return socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{Command: "/deploy"},
	}
}

func TestBuffer_AddAndLen(t *testing.T) {
	buf, _ := newTestBuffer(t, 2)

	if buf.Len() != 0 {
		t.Fatalf("expected empty buffer, got len=%d", buf.Len())
	}
	if !buf.Add(slashCommandEvent()) {
		t.Fatal("expected Add to return true when buffer has space")
	}
	if buf.Len() != 1 {
		t.Errorf("Len() = %d, want 1", buf.Len())
	}
}

func TestBuffer_Add_ReturnsFalseWhenFull(t *testing.T) {
	buf, _ := newTestBuffer(t, 1)

	if !buf.Add(slashCommandEvent()) {
		t.Fatal("expected first Add to succeed")
	}
	if buf.Add(slashCommandEvent()) {
		t.Fatal("expected Add to return false when buffer is full")
	}
}

func TestBuffer_DefaultSize(t *testing.T) {
	buf, _ := newTestBuffer(t, 0)
	if cap(buf.ch) != DefaultSize {
		t.Errorf("cap = %d, want DefaultSize=%d", cap(buf.ch), DefaultSize)
	}
}

func TestBuffer_Run_DrainsToRedis(t *testing.T) {
	buf, rdb := newTestBuffer(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buf.Add(slashCommandEvent())
	buf.Add(slashCommandEvent())

	go buf.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := rdb.XLen(ctx, queue.StreamKey).Result()
		if n >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for buffer to drain into Redis stream")
}

// TestBuffer_RedisDownThenRecovers verifies that events added to the buffer
// while Redis is unavailable are all delivered once Redis recovers, with no
// duplicates.
func TestBuffer_RedisDownThenRecovers(t *testing.T) {
	const n = 3
	buf, rdb, mr := newTestBufferWithMR(t, 10)

	// Simulate Redis outage before starting the drain loop.
	mr.SetError("connection refused")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go buf.Run(ctx)

	for i := 0; i < n; i++ {
		buf.Add(slashCommandEvent())
	}

	// With Redis down, nothing should reach the stream.
	time.Sleep(200 * time.Millisecond)
	got, _ := rdb.XLen(ctx, queue.StreamKey).Result()
	if got != 0 {
		t.Errorf("expected 0 events in stream while Redis is down, got %d", got)
	}

	// Recover Redis — the buffer must drain completely.
	mr.SetError("")

	// The first retry fires after a 1s backoff; subsequent events succeed
	// immediately. Allow 5s total.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = rdb.XLen(ctx, queue.StreamKey).Result()
		if got == int64(n) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected %d events in stream after recovery, got %d", n, got)
}

// TestBuffer_FullDuringOutage verifies that when the buffer fills during a
// Redis outage, overflow events are dropped (Add returns false) but all
// buffered events still drain correctly once Redis recovers.
func TestBuffer_FullDuringOutage(t *testing.T) {
	const capacity = 3
	buf, rdb, mr := newTestBufferWithMR(t, capacity)

	mr.SetError("connection refused")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go buf.Run(ctx)

	// Fill the buffer to capacity.
	for i := 0; i < capacity; i++ {
		if !buf.Add(slashCommandEvent()) {
			t.Fatalf("expected Add to succeed for event %d (capacity %d)", i, capacity)
		}
	}

	// One more event must be dropped.
	if buf.Add(slashCommandEvent()) {
		t.Error("expected Add to return false when buffer is full")
	}

	// Recover and wait for exactly capacity events to drain — not capacity+1.
	mr.SetError("")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := rdb.XLen(ctx, queue.StreamKey).Result()
		if got == int64(capacity) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	got, _ := rdb.XLen(ctx, queue.StreamKey).Result()
	t.Fatalf("expected %d events in stream after recovery, got %d", capacity, got)
}

// TestBuffer_ContextCancelledDuringRetry verifies that cancelling the context
// while the buffer is retrying against an unavailable Redis causes Run to exit
// cleanly without hanging.
func TestBuffer_ContextCancelledDuringRetry(t *testing.T) {
	buf, _, mr := newTestBufferWithMR(t, 10)

	mr.SetError("connection refused")

	ctx, cancel := context.WithCancel(context.Background())

	buf.Add(slashCommandEvent())

	done := make(chan struct{})
	go func() {
		buf.Run(ctx)
		close(done)
	}()

	// Let the first enqueue attempt fail and backoff begin.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Run exited cleanly.
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}
}
