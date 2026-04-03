package queue

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"
)

func newTestClient(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

// --- decode ---

func TestDecode_SlashCommand(t *testing.T) {
	cmd := slack.SlashCommand{Command: "/deploy", Text: "myapp"}
	evt := socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: cmd,
	}

	rdb := newTestClient(t)
	ctx := context.Background()

	if err := Enqueue(ctx, rdb, evt); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	msgs, err := rdb.XRange(ctx, StreamKey, "-", "+").Result()
	if err != nil || len(msgs) == 0 {
		t.Fatalf("expected 1 message in stream, got err=%v msgs=%d", err, len(msgs))
	}

	got, err := decode(msgs[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != socketmode.EventTypeSlashCommand {
		t.Errorf("type = %q, want %q", got.Type, socketmode.EventTypeSlashCommand)
	}
	gotCmd, ok := got.Data.(slack.SlashCommand)
	if !ok {
		t.Fatal("Data is not slack.SlashCommand")
	}
	if gotCmd.Command != "/deploy" || gotCmd.Text != "myapp" {
		t.Errorf("command = %+v, want {Command:/deploy Text:myapp}", gotCmd)
	}
}

func TestDecode_InteractionCallback(t *testing.T) {
	cb := slack.InteractionCallback{Type: slack.InteractionTypeBlockActions}
	evt := socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: cb,
	}

	rdb := newTestClient(t)
	ctx := context.Background()

	if err := Enqueue(ctx, rdb, evt); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	msgs, err := rdb.XRange(ctx, StreamKey, "-", "+").Result()
	if err != nil || len(msgs) == 0 {
		t.Fatalf("expected 1 message in stream")
	}

	got, err := decode(msgs[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != socketmode.EventTypeInteractive {
		t.Errorf("type = %q, want %q", got.Type, socketmode.EventTypeInteractive)
	}
	if _, ok := got.Data.(slack.InteractionCallback); !ok {
		t.Fatal("Data is not slack.InteractionCallback")
	}
}

func TestDecode_ECRPushEvent(t *testing.T) {
	evt := NewECRPushEvent(ECRPushEvent{
		App:        "myapp",
		Tag:        "v1.0.0",
		Repository: "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp",
	})

	rdb := newTestClient(t)
	ctx := context.Background()

	if err := Enqueue(ctx, rdb, evt); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	msgs, err := rdb.XRange(ctx, StreamKey, "-", "+").Result()
	if err != nil || len(msgs) == 0 {
		t.Fatalf("expected 1 message in stream, got err=%v msgs=%d", err, len(msgs))
	}

	got, err := decode(msgs[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != EventTypeECRPush {
		t.Errorf("type = %q, want %q", got.Type, EventTypeECRPush)
	}
	ecrEvt, ok := got.Data.(ECRPushEvent)
	if !ok {
		t.Fatal("Data is not ECRPushEvent")
	}
	if ecrEvt.App != "myapp" || ecrEvt.Tag != "v1.0.0" {
		t.Errorf("ecr event = %+v, want {App:myapp Tag:v1.0.0}", ecrEvt)
	}
}

func TestDecode_UnknownType(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()

	// Manually insert an envelope with an unsupported type.
	rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamKey,
		Values: map[string]interface{}{"payload": `{"type":"unknown","data":{}}`},
	})

	msgs, _ := rdb.XRange(ctx, StreamKey, "-", "+").Result()
	_, err := decode(msgs[0])
	if err == nil {
		t.Fatal("expected error for unknown event type")
	}
}

func TestDecode_MalformedPayload(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()

	rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamKey,
		Values: map[string]interface{}{"payload": `not json`},
	})

	msgs, _ := rdb.XRange(ctx, StreamKey, "-", "+").Result()
	_, err := decode(msgs[0])
	if err == nil {
		t.Fatal("expected error for malformed payload")
	}
}

func TestDecode_MissingPayloadField(t *testing.T) {
	rdb := newTestClient(t)
	ctx := context.Background()

	rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamKey,
		Values: map[string]interface{}{"other_field": "value"},
	})

	msgs, _ := rdb.XRange(ctx, StreamKey, "-", "+").Result()
	_, err := decode(msgs[0])
	if err == nil {
		t.Fatal("expected error for missing payload field")
	}
}

// --- Worker ---

func newTestWorker(t *testing.T, rdb *redis.Client) *Worker {
	t.Helper()
	return &Worker{
		rdb:      rdb,
		consumer: "test-consumer",
		log:      zap.NewNop(),
	}
}

func TestWorker_Init_CreatesConsumerGroup(t *testing.T) {
	rdb := newTestClient(t)
	w := newTestWorker(t, rdb)
	ctx := context.Background()

	if err := w.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Calling Init again must not error (group already exists).
	if err := w.Init(ctx); err != nil {
		t.Fatalf("second Init: %v", err)
	}
}

func TestWorker_ProcessesEnqueuedEvent(t *testing.T) {
	rdb := newTestClient(t)
	w := newTestWorker(t, rdb)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := w.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	cmd := slack.SlashCommand{Command: "/deploy", Text: "status"}
	if err := Enqueue(ctx, rdb, socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: cmd,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	handled := make(chan socketmode.Event, 1)
	go w.Run(ctx, func(_ context.Context, evt socketmode.Event) {
		handled <- evt
		cancel() // stop the worker after the first event
	})

	select {
	case got := <-handled:
		gotCmd, ok := got.Data.(slack.SlashCommand)
		if !ok {
			t.Fatal("Data is not slack.SlashCommand")
		}
		if gotCmd.Command != "/deploy" || gotCmd.Text != "status" {
			t.Errorf("got %+v, want {Command:/deploy Text:status}", gotCmd)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event to be handled")
	}
}

func TestWorker_AcksAfterHandle(t *testing.T) {
	rdb := newTestClient(t)
	w := newTestWorker(t, rdb)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := w.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	Enqueue(ctx, rdb, socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{Command: "/deploy"},
	})

	done := make(chan struct{})
	go w.Run(ctx, func(_ context.Context, _ socketmode.Event) {
		close(done)
		cancel()
	})

	<-done
	// Give the worker a moment to XACK before checking.
	time.Sleep(50 * time.Millisecond)

	pending, err := rdb.XPending(context.Background(), StreamKey, ConsumerGroup).Result()
	if err != nil {
		t.Fatalf("xpending: %v", err)
	}
	if pending.Count != 0 {
		t.Errorf("pending count = %d after handle, want 0", pending.Count)
	}
}

// TestTwoWorkers_NoDoubleDelivery starts two workers with distinct consumer
// names against the same stream and verifies that N enqueued events are each
// handled exactly once (no double delivery, no dropped messages).
func TestTwoWorkers_NoDoubleDelivery(t *testing.T) {
	const n = 20
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w1 := NewWorkerWithName(rdb, "worker-1", zap.NewNop())
	w2 := NewWorkerWithName(rdb, "worker-2", zap.NewNop())

	if err := w1.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Enqueue all events before starting the workers.
	for i := 0; i < n; i++ {
		if err := Enqueue(ctx, rdb, socketmode.Event{
			Type: socketmode.EventTypeSlashCommand,
			Data: slack.SlashCommand{Command: "/deploy", Text: "status"},
		}); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	var total atomic.Int32
	handle := func(_ context.Context, _ socketmode.Event) {
		if total.Add(1) == n {
			cancel() // stop both workers once all events are accounted for
		}
	}

	go w1.Run(ctx, handle)
	go w2.Run(ctx, handle)

	select {
	case <-ctx.Done():
		// all n events handled
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out: only %d of %d events handled", total.Load(), n)
	}

	if got := total.Load(); got != n {
		t.Errorf("handle called %d times, want %d (no double delivery, no drops)", got, n)
	}

	// Verify the PEL is empty — all messages ACKed.
	time.Sleep(50 * time.Millisecond)
	pending, err := rdb.XPending(context.Background(), StreamKey, ConsumerGroup).Result()
	if err != nil {
		t.Fatalf("xpending: %v", err)
	}
	if pending.Count != 0 {
		t.Errorf("pending count = %d after all events handled, want 0", pending.Count)
	}
}

func TestWorker_MalformedMessageIsAckedAndSkipped(t *testing.T) {
	rdb := newTestClient(t)
	w := newTestWorker(t, rdb)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := w.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Insert a bad message followed by a good one.
	rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamKey,
		Values: map[string]interface{}{"payload": "not json at all"},
	})
	Enqueue(ctx, rdb, socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{Command: "/deploy"},
	})

	var callCount atomic.Int32
	done := make(chan struct{})
	go w.Run(ctx, func(_ context.Context, _ socketmode.Event) {
		if callCount.Add(1) == 1 {
			close(done)
			cancel()
		}
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out — malformed message may have blocked the worker")
	}

	if n := callCount.Load(); n != 1 {
		t.Errorf("handle called %d times, want 1 (malformed message should be skipped)", n)
	}
}
