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
