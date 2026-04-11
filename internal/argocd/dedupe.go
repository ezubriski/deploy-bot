package argocd

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// dedupeTTL is the lifetime of a "we already enqueued this notification"
// marker. 24h is long enough to absorb argocd-notifications controller
// restarts (which can replay recent events because state is in-process)
// and our own at-least-once stream semantics (XAutoClaim of stuck
// messages), but short enough that the dedupe key set does not grow
// unboundedly.
const dedupeTTL = 24 * time.Hour

// Deduper is a Redis-backed "have we already accepted this notification"
// check. The key is a tuple of (argocd app, gitops sha, trigger), so a
// retry of the same trigger for the same revision suppresses, but a new
// trigger or a new revision passes through. The check uses SET NX, which
// is atomic and survives concurrent receivers in a multi-replica deploy.
type Deduper struct {
	rdb *redis.Client
}

// NewDeduper returns a Deduper backed by the given Redis client.
func NewDeduper(rdb *redis.Client) *Deduper {
	return &Deduper{rdb: rdb}
}

// dedupeKey returns the Redis key for a notification's dedupe marker.
// Empty fields are tolerated (they just produce a key with empty
// segments) so a malformed payload still hashes consistently.
func dedupeKey(app, sha, trigger string) string {
	return fmt.Sprintf("argocd:seen:%s:%s:%s", app, sha, trigger)
}

// Accept reports whether this notification should be processed (true) or
// suppressed as a duplicate (false). On the first call for a given
// (app, sha, trigger) tuple it sets the marker and returns true. On any
// subsequent call within dedupeTTL it returns false. A Redis error is
// surfaced to the caller — fail closed (do not enqueue) is safer than
// fail open (double-post degraded alarms), and the controller will retry
// on a 5xx anyway.
//
// Uses SetArgs with NX rather than SetNX to match the rest of the
// codebase (store.AcquireLock, store.TryLock, store.SetThreadTS) — SetNX
// is deprecated as of Redis 2.6.12 in favor of SET with the NX option.
func (d *Deduper) Accept(ctx context.Context, app, sha, trigger string) (bool, error) {
	err := d.rdb.SetArgs(ctx, dedupeKey(app, sha, trigger), "1", redis.SetArgs{
		Mode: "NX",
		TTL:  dedupeTTL,
	}).Err()
	if err == redis.Nil {
		return false, nil // key already exists — duplicate
	}
	if err != nil {
		return false, fmt.Errorf("argocd dedupe: %w", err)
	}
	return true, nil
}
