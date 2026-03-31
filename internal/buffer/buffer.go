package buffer

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/queue"
)

const (
	DefaultSize = 500
	maxBackoff  = 30 * time.Second
)

// Buffer holds events that could not be enqueued to Redis and retries them
// with exponential backoff until Redis recovers. Events are never ACKed to
// Slack from the buffer — Slack continues retrying unACKed events in parallel,
// providing a second delivery path if the receiver restarts.
type Buffer struct {
	ch  chan socketmode.Event
	rdb *redis.Client
	log *zap.Logger
}

func New(size int, rdb *redis.Client, log *zap.Logger) *Buffer {
	if size <= 0 {
		size = DefaultSize
	}
	return &Buffer{
		ch:  make(chan socketmode.Event, size),
		rdb: rdb,
		log: log,
	}
}

// Add places an event in the buffer. Returns false and drops the event if the
// buffer is full — Slack will retry delivery since the event was never ACKed.
func (b *Buffer) Add(evt socketmode.Event) bool {
	select {
	case b.ch <- evt:
		b.log.Warn("buffer: event queued for retry",
			zap.String("type", string(evt.Type)),
			zap.Int("buffered", len(b.ch)),
		)
		return true
	default:
		b.log.Error("buffer: full, dropping event — Slack will retry",
			zap.String("type", string(evt.Type)),
			zap.Int("capacity", cap(b.ch)),
		)
		return false
	}
}

// Len returns the number of events currently waiting in the buffer.
func (b *Buffer) Len() int { return len(b.ch) }

// Run drains the buffer until ctx is cancelled. Each event is retried with
// exponential backoff (1s → 30s) until successfully enqueued to Redis.
func (b *Buffer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-b.ch:
			b.drainOne(ctx, evt)
		}
	}
}

func (b *Buffer) drainOne(ctx context.Context, evt socketmode.Event) {
	backoff := time.Second
	for {
		if err := queue.Enqueue(ctx, b.rdb, evt); err == nil {
			b.log.Info("buffer: event drained", zap.String("type", string(evt.Type)))
			return
		}
		b.log.Warn("buffer: enqueue failed, retrying",
			zap.String("type", string(evt.Type)),
			zap.Duration("backoff", backoff),
		)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff = min(backoff*2, maxBackoff)
		}
	}
}
