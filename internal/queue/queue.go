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
	// StreamKeyUser carries user-initiated events (slash commands, interactions, mentions).
	StreamKeyUser = "user:events"
	// StreamKeyECR carries ECR push events.
	StreamKeyECR = "ecr:events"
	// StreamKeyArgoCD carries ArgoCD lifecycle webhook notifications
	// (sync-succeeded, sync-failed, health-degraded). Drained at the same
	// priority tier as ECR — after the user stream — so an interactive
	// click is never delayed by an inbound ArgoCD burst.
	StreamKeyArgoCD = "argocd:events"

	// StreamKey is kept for backward compatibility. New code should use
	// StreamKeyUser, StreamKeyECR, or StreamKeyArgoCD.
	StreamKey = StreamKeyUser

	ConsumerGroup = "bot-workers"
	streamMaxLen  = 10_000
	claimMinIdle  = 60 * time.Second
	readTimeout   = 5 * time.Second
	batchSize     = 10
)

// AllStreams is the list of streams the worker reads from. User stream is
// listed first so it has priority when both background streams have pending
// messages; ECR and ArgoCD share the second tier.
var AllStreams = []string{StreamKeyUser, StreamKeyECR, StreamKeyArgoCD}

// envelope carries the event type alongside the raw JSON payload so the
// worker can reconstruct the concrete socketmode.Event.Data type.
type envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// EnqueueTo serializes evt and appends it to the given Redis stream.
func EnqueueTo(ctx context.Context, rdb *redis.Client, streamKey string, evt socketmode.Event) error {
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
		Stream: streamKey,
		MaxLen: streamMaxLen,
		Approx: true,
		Values: map[string]interface{}{"payload": string(payload)},
	}).Err()
}

// Enqueue appends an event to the user stream. Kept for backward compatibility.
func Enqueue(ctx context.Context, rdb *redis.Client, evt socketmode.Event) error {
	return EnqueueUser(ctx, rdb, evt)
}

// EnqueueUser appends an event to the user stream (slash commands, interactions, mentions).
func EnqueueUser(ctx context.Context, rdb *redis.Client, evt socketmode.Event) error {
	return EnqueueTo(ctx, rdb, StreamKeyUser, evt)
}

// EnqueueECR appends an event to the ECR stream.
func EnqueueECR(ctx context.Context, rdb *redis.Client, evt socketmode.Event) error {
	return EnqueueTo(ctx, rdb, StreamKeyECR, evt)
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
	case EventTypeArgoCDNotification:
		var argo ArgoCDNotificationEvent
		if err := json.Unmarshal(env.Data, &argo); err != nil {
			return socketmode.Event{}, fmt.Errorf("unmarshal argocd notification event: %w", err)
		}
		evt.Data = argo
	default:
		return socketmode.Event{}, fmt.Errorf("unsupported event type: %s", env.Type)
	}
	return evt, nil
}

const (
	// DefaultECRConcurrency is the default number of ECR events processed
	// concurrently per worker.
	DefaultECRConcurrency = 10
)

// Worker reads events from the streams and dispatches them.
type Worker struct {
	rdb            *redis.Client
	consumer       string
	ecrConcurrency int
	log            *zap.Logger
}

func NewWorker(rdb *redis.Client, log *zap.Logger) *Worker {
	consumer, err := os.Hostname()
	if err != nil {
		log.Warn("queue: get hostname for consumer name, using fallback", zap.Error(err))
	}
	if consumer == "" {
		consumer = "worker"
	}
	return NewWorkerWithName(rdb, consumer, log)
}

// NewWorkerWithName creates a Worker with an explicit consumer name. Useful
// when running multiple workers in the same process (e.g. tests) where
// os.Hostname would produce the same name for all of them.
func NewWorkerWithName(rdb *redis.Client, name string, log *zap.Logger) *Worker {
	return &Worker{rdb: rdb, consumer: name, ecrConcurrency: DefaultECRConcurrency, log: log}
}

// SetECRConcurrency sets the maximum number of ECR events processed
// concurrently. Must be called before Run.
func (w *Worker) SetECRConcurrency(n int) {
	if n > 0 {
		w.ecrConcurrency = n
	}
}

// Init creates the consumer group on all streams, tolerating the error if it
// already exists.
func (w *Worker) Init(ctx context.Context) error {
	for _, stream := range AllStreams {
		err := w.rdb.XGroupCreateMkStream(ctx, stream, ConsumerGroup, "0").Err()
		if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
			return fmt.Errorf("create consumer group on %s: %w", stream, err)
		}
	}
	return nil
}

// Run reads from both streams and calls handle for each event until ctx is
// cancelled. The user stream is listed first so user actions take priority
// over ECR bulk when both streams have pending messages. It reclaims messages
// idle for over claimMinIdle every 30s so that events stuck on a crashed
// worker are not lost.
func (w *Worker) Run(ctx context.Context, handle func(context.Context, socketmode.Event)) {
	claimTicker := time.NewTicker(30 * time.Second)
	defer claimTicker.Stop()

	ecrSem := make(chan struct{}, w.ecrConcurrency)

	for {
		// Check for stuck messages on the ticker, non-blocking.
		select {
		case <-ctx.Done():
			return
		case <-claimTicker.C:
			w.reclaimStuck(ctx, handle)
		default:
		}

		// Priority read: drain user stream first, then background streams
		// (ECR and ArgoCD). This ensures interactive events (button clicks,
		// modal submits) are never delayed by an ECR bulk backlog or an
		// ArgoCD notification burst.
		if w.readStream(ctx, StreamKeyUser, handle, nil) {
			continue // user stream had messages — check it again before background streams
		}
		w.readStream(ctx, StreamKeyECR, handle, ecrSem)
		// ArgoCD events are processed sequentially: volume is naturally
		// low (one notification per app per sync) and the worker-side
		// handler does its own dedupe + lookups, so unbounded concurrency
		// would only multiply Redis round trips for no real throughput
		// gain.
		w.readStream(ctx, StreamKeyArgoCD, handle, nil)
	}
}

// readStream reads a batch from a single stream and processes messages.
// If sem is non-nil, messages are processed concurrently using the semaphore
// to bound concurrency. If sem is nil, messages are processed sequentially.
// Returns true if any messages were read.
func (w *Worker) readStream(ctx context.Context, streamKey string, handle func(context.Context, socketmode.Event), sem chan struct{}) bool {
	msgs, err := w.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    ConsumerGroup,
		Consumer: w.consumer,
		Streams:  []string{streamKey, ">"},
		Count:    batchSize,
		Block:    time.Second, // short block so we cycle back to check user stream
	}).Result()
	if err != nil {
		if err == context.Canceled || err == redis.Nil {
			return false
		}
		if strings.Contains(err.Error(), "NOGROUP") {
			w.log.Warn("queue: consumer group missing, re-initializing", zap.String("stream", streamKey))
			if initErr := w.Init(ctx); initErr != nil {
				w.log.Error("queue: re-init consumer group", zap.Error(initErr))
			}
			return false
		}
		w.log.Error("queue: xreadgroup", zap.String("stream", streamKey), zap.Error(err))
		return false
	}

	processed := false
	for _, stream := range msgs {
		for _, msg := range stream.Messages {
			if sem != nil {
				// Concurrent: acquire semaphore slot, process in goroutine.
				sem <- struct{}{}
				go func(m redis.XMessage) {
					defer func() { <-sem }()
					w.process(ctx, streamKey, m, handle)
				}(msg)
			} else {
				// Sequential: process inline.
				w.process(ctx, streamKey, msg, handle)
			}
			processed = true
		}
	}
	return processed
}

func (w *Worker) process(ctx context.Context, streamKey string, msg redis.XMessage, handle func(context.Context, socketmode.Event)) {
	evt, err := decode(msg)
	if err != nil {
		w.log.Error("queue: decode message", zap.String("stream", streamKey), zap.String("id", msg.ID), zap.Error(err))
		// ACK malformed messages so they don't block the queue.
		_ = w.rdb.XAck(context.Background(), streamKey, ConsumerGroup, msg.ID)
		return
	}
	handle(ctx, evt)
	// Use Background context for ACK: this is a cleanup operation that must
	// succeed even if the worker is shutting down and ctx is already canceled.
	if err := w.rdb.XAck(context.Background(), streamKey, ConsumerGroup, msg.ID).Err(); err != nil {
		w.log.Error("queue: xack", zap.String("stream", streamKey), zap.String("id", msg.ID), zap.Error(err))
	}
}

func (w *Worker) reclaimStuck(ctx context.Context, handle func(context.Context, socketmode.Event)) {
	for _, stream := range AllStreams {
		msgs, _, err := w.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   stream,
			Group:    ConsumerGroup,
			Consumer: w.consumer,
			MinIdle:  claimMinIdle,
			Start:    "0-0",
			Count:    batchSize,
		}).Result()
		if err != nil {
			w.log.Error("queue: xautoclaim", zap.String("stream", stream), zap.Error(err))
			continue
		}
		for _, msg := range msgs {
			w.process(ctx, stream, msg, handle)
		}
	}
}
