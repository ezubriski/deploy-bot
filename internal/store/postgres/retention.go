package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// deleteBatchSize caps how many rows a single DELETE removes to avoid
// holding a long-running transaction on a large table. The retention
// loop repeats until a batch deletes fewer than this many rows.
const deleteBatchSize = 1000

// Retainer purges expired history rows on a periodic tick. Intended
// to run as a background goroutine in the bot process (not the
// receiver — only one component should own retention).
//
// Each tick:
//  1. Logs a pre-flight count of rows that would be deleted, broken
//     down by event_type. Operators watching the log can spot a
//     misconfigured retention window before the DELETE runs.
//  2. Deletes in batches of deleteBatchSize until no qualifying rows
//     remain. Each batch is its own transaction so a slow purge
//     doesn't hold locks for minutes.
//  3. Records the total deleted count as a metric.
//
// Coordination: the caller is expected to gate RunOnce behind a Redis
// sys-lock (same pattern as the sweeper) so only one bot replica runs
// retention at a time in a multi-replica deployment.
type Retainer struct {
	pool      *pgxpool.Pool
	retention time.Duration
	log       *zap.Logger
}

// NewRetainer constructs a Retainer. retention is the maximum age of
// a history row — anything whose completed_at is older than
// time.Now().Add(-retention) is eligible for deletion. The caller
// (cmd/bot/main.go) typically passes
// cfg.Postgres.HistoryRetentionDuration() which enforces the 390-day
// floor at config load time.
func NewRetainer(pool *pgxpool.Pool, retention time.Duration, log *zap.Logger) *Retainer {
	if log == nil {
		log = zap.NewNop()
	}
	return &Retainer{pool: pool, retention: retention, log: log}
}

// RunOnce executes a single retention pass. Safe to call concurrently
// from tests; in production, called by the bot's ticker goroutine
// under a Redis sys-lock.
func (r *Retainer) RunOnce(ctx context.Context) {
	cutoff := time.Now().Add(-r.retention)

	// Pre-flight: how many rows will we delete, by event_type?
	preflight, err := r.preflightCount(ctx, cutoff)
	if err != nil {
		r.log.Error("retention: preflight count", zap.Error(err))
		return
	}
	if preflight.total == 0 {
		r.log.Debug("retention: nothing to purge",
			zap.Time("cutoff", cutoff),
		)
		return
	}

	r.log.Info("retention: starting purge",
		zap.Time("cutoff", cutoff),
		zap.Int64("total_eligible", preflight.total),
		zap.Any("by_event_type", preflight.byType),
	)

	deleted, err := r.deleteBatched(ctx, cutoff)
	if err != nil {
		r.log.Error("retention: delete",
			zap.Int64("deleted_before_error", deleted),
			zap.Error(err),
		)
		return
	}

	r.log.Info("retention: purge complete",
		zap.Int64("deleted", deleted),
		zap.Time("cutoff", cutoff),
	)
}

type preflightResult struct {
	total  int64
	byType map[string]int64
}

func (r *Retainer) preflightCount(ctx context.Context, cutoff time.Time) (preflightResult, error) {
	const q = `SELECT event_type, COUNT(*) FROM history
		WHERE completed_at < $1
		GROUP BY event_type`
	rows, err := r.pool.Query(ctx, q, cutoff)
	if err != nil {
		return preflightResult{}, fmt.Errorf("preflight: %w", err)
	}
	defer rows.Close()

	res := preflightResult{byType: make(map[string]int64)}
	for rows.Next() {
		var eventType string
		var count int64
		if err := rows.Scan(&eventType, &count); err != nil {
			return preflightResult{}, fmt.Errorf("preflight scan: %w", err)
		}
		res.byType[eventType] = count
		res.total += count
	}
	return res, rows.Err()
}

func (r *Retainer) deleteBatched(ctx context.Context, cutoff time.Time) (int64, error) {
	// The subquery + LIMIT pattern avoids a full-table lock on large
	// deletes. Each batch acquires row locks only on the rows it's
	// about to delete. The history_completed_at_idx index keeps the
	// subquery scan tight.
	const q = `DELETE FROM history
		WHERE id IN (
			SELECT id FROM history
			WHERE completed_at < $1
			LIMIT $2
		)`

	var totalDeleted int64
	for {
		tag, err := r.pool.Exec(ctx, q, cutoff, deleteBatchSize)
		if err != nil {
			return totalDeleted, fmt.Errorf("retention batch: %w", err)
		}
		n := tag.RowsAffected()
		totalDeleted += n
		if n < deleteBatchSize {
			break // last batch — fewer rows than the limit means we're done
		}
	}
	return totalDeleted, nil
}
