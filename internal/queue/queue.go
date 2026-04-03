package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"
)

const (
	StreamKey     = "slack:events"
	ConsumerGroup = "bot-workers"
	streamMaxLen  = 10_000
	claimMinIdle  = 60 * time.Second
	readTimeout   = 5 * time.Second
	batchSize     = 10
)

// envelope carries the event type alongside the raw JSON payload so the
// worker can reconstruct the concrete socketmode.Event.Data type.
type envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Enqueue serializes evt and appends it to the Redis stream.
func Enqueue(ctx context.Context, rdb *redis.Client, evt socketmode.Event) error {
	data, err := json.Marshal(evt.Data)
	if err != nil {
		return fmt.Errorf("marshal event data: %w", err)
	}
	env := envelope{Type: string(evt.Type), Data: data}
	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	return rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamKey,
		MaxLen: streamMaxLen,
		Approx: true,
		Values: map[string]interface{}{"payload": string(payload)},
	}).Err()
}

// decode reconstructs a socketmode.Event from a stream message.
func decode(msg redis.XMessage) (socketmode.Event, error) {
	raw, ok := msg.Values["payload"].(string)
	if !ok {
		return socketmode.Event{}, fmt.Errorf("missing payload field")
	}
	var env envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return socketmode.Event{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	evt := socketmode.Event{Type: socketmode.EventType(env.Type)}
	switch evt.Type {
	case socketmode.EventTypeSlashCommand:
		var cmd slack.SlashCommand
		if err := json.Unmarshal(env.Data, &cmd); err != nil {
			return socketmode.Event{}, fmt.Errorf("unmarshal slash command: %w", err)
		}
		evt.Data = cmd
	case socketmode.EventTypeInteractive:
		var cb slack.InteractionCallback
		if err := json.Unmarshal(env.Data, &cb); err != nil {
			return socketmode.Event{}, fmt.Errorf("unmarshal interaction: %w", err)
		}
		evt.Data = cb
	case EventTypeECRPush:
		var ecr ECRPushEvent
		if err := json.Unmarshal(env.Data, &ecr); err != nil {
			return socketmode.Event{}, fmt.Errorf("unmarshal ecr push event: %w", err)
		}
		evt.Data = ecr
	case EventTypeAppMention:
		var mention AppMentionEvent
		if err := json.Unmarshal(env.Data, &mention); err != nil {
			return socketmode.Event{}, fmt.Errorf("unmarshal app mention event: %w", err)
		}
		evt.Data = mention
	default:
		return socketmode.Event{}, fmt.Errorf("unsupported event type: %s", env.Type)
	}
	return evt, nil
}

// Worker reads events from the stream and dispatches them.
type Worker struct {
	rdb      *redis.Client
	consumer string
	log      *zap.Logger
}

func NewWorker(rdb *redis.Client, log *zap.Logger) *Worker {
	consumer, _ := os.Hostname()
	if consumer == "" {
		consumer = "worker"
	}
	return NewWorkerWithName(rdb, consumer, log)
}

// NewWorkerWithName creates a Worker with an explicit consumer name. Useful
// when running multiple workers in the same process (e.g. tests) where
// os.Hostname would produce the same name for all of them.
func NewWorkerWithName(rdb *redis.Client, name string, log *zap.Logger) *Worker {
	return &Worker{rdb: rdb, consumer: name, log: log}
}

// Init creates the consumer group, tolerating the error if it already exists.
func (w *Worker) Init(ctx context.Context) error {
	err := w.rdb.XGroupCreateMkStream(ctx, StreamKey, ConsumerGroup, "0").Err()
	if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
		return fmt.Errorf("create consumer group: %w", err)
	}
	return nil
}

// Run reads from the stream and calls handle for each event until ctx is
// cancelled. It reclaims messages idle for over claimMinIdle every 30s so
// that events stuck on a crashed worker are not lost.
func (w *Worker) Run(ctx context.Context, handle func(context.Context, socketmode.Event)) {
	claimTicker := time.NewTicker(30 * time.Second)
	defer claimTicker.Stop()

	for {
		// Check for stuck messages on the ticker, non-blocking.
		select {
		case <-ctx.Done():
			return
		case <-claimTicker.C:
			w.reclaimStuck(ctx, handle)
		default:
		}

		msgs, err := w.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    ConsumerGroup,
			Consumer: w.consumer,
			Streams:  []string{StreamKey, ">"},
			Count:    batchSize,
			Block:    readTimeout,
		}).Result()
		if err != nil {
			if err == context.Canceled || err == redis.Nil {
				continue
			}
			// Consumer group was removed (e.g. Redis flush). Re-initialise and continue.
			if strings.Contains(err.Error(), "NOGROUP") {
				w.log.Warn("queue: consumer group missing, re-initializing")
				if initErr := w.Init(ctx); initErr != nil {
					w.log.Error("queue: re-init consumer group", zap.Error(initErr))
				}
				continue
			}
			w.log.Error("queue: xreadgroup", zap.Error(err))
			continue
		}

		for _, stream := range msgs {
			for _, msg := range stream.Messages {
				w.process(ctx, msg, handle)
			}
		}
	}
}

func (w *Worker) process(ctx context.Context, msg redis.XMessage, handle func(context.Context, socketmode.Event)) {
	evt, err := decode(msg)
	if err != nil {
		w.log.Error("queue: decode message", zap.String("id", msg.ID), zap.Error(err))
		// ACK malformed messages so they don't block the queue.
		_ = w.rdb.XAck(context.Background(), StreamKey, ConsumerGroup, msg.ID)
		return
	}
	handle(ctx, evt)
	// Use Background context for ACK: this is a cleanup operation that must
	// succeed even if the worker is shutting down and ctx is already canceled.
	if err := w.rdb.XAck(context.Background(), StreamKey, ConsumerGroup, msg.ID).Err(); err != nil {
		w.log.Error("queue: xack", zap.String("id", msg.ID), zap.Error(err))
	}
}

func (w *Worker) reclaimStuck(ctx context.Context, handle func(context.Context, socketmode.Event)) {
	msgs, _, err := w.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   StreamKey,
		Group:    ConsumerGroup,
		Consumer: w.consumer,
		MinIdle:  claimMinIdle,
		Start:    "0-0",
		Count:    batchSize,
	}).Result()
	if err != nil {
		w.log.Error("queue: xautoclaim", zap.Error(err))
		return
	}
	for _, msg := range msgs {
		w.process(ctx, msg, handle)
	}
}
