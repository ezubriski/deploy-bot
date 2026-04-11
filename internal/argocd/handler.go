package argocd

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/buffer"
	"github.com/ezubriski/deploy-bot/internal/queue"
)

// Processor takes a parsed WebhookPayload, dedupes it, and enqueues an
// ArgoCDNotificationEvent on the argocd:events stream for the worker to
// process. It is called from the HTTP handler after the body has been
// validated and parsed.
//
// Enqueue failures fall through to the in-memory buffer (same pattern as
// the ECR webhook), so a transient Redis blip does not lose
// notifications — the buffer drains them on recovery.
type Processor struct {
	rdb     *redis.Client
	buf     *buffer.Buffer
	deduper *Deduper
	log     *zap.Logger
}

// NewProcessor constructs a Processor.
func NewProcessor(rdb *redis.Client, buf *buffer.Buffer, log *zap.Logger) *Processor {
	if log == nil {
		log = zap.NewNop()
	}
	return &Processor{
		rdb:     rdb,
		buf:     buf,
		deduper: NewDeduper(rdb),
		log:     log,
	}
}

// ProcessResult reports what the Processor did with a notification, used
// to shape the JSON body of the webhook 200 response and for tests.
type ProcessResult struct {
	// Recognized is true if the trigger was one this bot subscribes to.
	// Unrecognized triggers are accepted (200) but dropped without
	// enqueueing — see IsRecognizedTrigger for the rationale.
	Recognized bool

	// Deduped is true if the notification was suppressed because we have
	// already enqueued an identical (app, sha, trigger) tuple within the
	// dedupe window. The webhook still returns 200.
	Deduped bool

	// Enqueued is true if an event was successfully written to either the
	// Redis stream or the retry buffer.
	Enqueued bool
}

// Process applies the dedupe + enqueue pipeline to a parsed payload. It
// always returns a ProcessResult; an error is only returned for genuine
// infrastructure failures (Redis dedupe call) where the controller should
// be told to retry. Stream-write failures fall through to the buffer and
// are not surfaced as errors.
func (p *Processor) Process(ctx context.Context, payload WebhookPayload) (ProcessResult, error) {
	res := ProcessResult{}

	if !IsRecognizedTrigger(payload.Trigger) {
		p.log.Debug("argocd: dropping unrecognized trigger",
			zap.String("trigger", payload.Trigger),
			zap.String("argocd_app", payload.ArgoCDApp),
		)
		return res, nil
	}
	res.Recognized = true

	// Phase 2 explicitly skips on-sync-running: the user decision was
	// "we should pass on information that is impactful and suggest
	// actions that are helpful" — sync-running is neither, and a
	// dead-man's-switch watchdog is the better shape for that signal.
	// Accepted by IsRecognizedTrigger so the controller does not log
	// "no recipient" errors, dropped here so we do not enqueue noise.
	if payload.Trigger == "on-sync-running" {
		p.log.Debug("argocd: dropping on-sync-running (not currently used)",
			zap.String("argocd_app", payload.ArgoCDApp),
		)
		return res, nil
	}

	// Dedupe before enqueue. The marker is set inside the SetNX so a
	// concurrent retry from a parallel receiver pod cannot race past the
	// check.
	ok, err := p.deduper.Accept(ctx, payload.ArgoCDApp, payload.SyncResultRevision, payload.Trigger)
	if err != nil {
		// Surface as an error so the HTTP handler returns 5xx and the
		// controller retries. Better than fail-open: a duplicate degraded
		// alert is more disruptive than a slightly delayed one.
		return res, fmt.Errorf("dedupe: %w", err)
	}
	if !ok {
		res.Deduped = true
		p.log.Debug("argocd: deduped repeat notification",
			zap.String("trigger", payload.Trigger),
			zap.String("argocd_app", payload.ArgoCDApp),
			zap.String("sha", payload.SyncResultRevision),
		)
		return res, nil
	}

	evt := queue.NewArgoCDNotificationEvent(queue.ArgoCDNotificationEvent{
		Trigger:         payload.Trigger,
		ArgoCDApp:       payload.ArgoCDApp,
		Namespace:       payload.Namespace,
		RepoURL:         payload.RepoURL,
		GitopsCommitSHA: payload.SyncResultRevision,
		SyncStatus:      payload.SyncStatus,
		HealthStatus:    payload.HealthStatus,
		Phase:           payload.Phase,
		Message:         payload.Message,
		Resources:       payload.Resources,
		ReceivedAt:      time.Now().UTC(),
	})

	if err := queue.EnqueueArgoCD(ctx, p.rdb, evt); err != nil {
		// Mirror the ECR webhook behavior: do not surface a stream
		// failure to the upstream controller. The buffer holds events in
		// memory and replays them on Redis recovery, which is preferable
		// to bouncing the controller into its own retry loop.
		p.log.Warn("argocd: enqueue failed, buffering for retry",
			zap.String("trigger", payload.Trigger),
			zap.String("argocd_app", payload.ArgoCDApp),
			zap.Error(err),
		)
		if p.buf != nil {
			p.buf.Add(evt)
		}
	}
	res.Enqueued = true
	return res, nil
}
