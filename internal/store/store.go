package store

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	lockPrefix    = "lock:"
	sysLockPrefix = "syslock:"
	threadPrefix  = "thread:"
)

// DefaultHistoryLimit is the default page size for unbounded history
// queries. Callers that want the full retained window should pass an
// explicit larger limit (retention is configurable per install via
// config.Postgres.RetentionHistory; two years is the default).
const DefaultHistoryLimit = 100

// Store wraps both backing stores the bot uses: Redis for ephemeral
// coordination (locks, thread timestamps, sweeper/reconcile leader
// locks, dedupe markers) and Postgres for durable structured state
// (deploy history, in-flight pending deploys).
//
// Either backing store can be nil in unit tests that don't exercise
// the methods it fronts: a test that only touches locks can pass
// nil for pg, and a test that only touches history can pass nil for
// rdb. The relevant methods will panic with a clear message if
// called with their required store missing — see the nil checks in
// each method.
type Store struct {
	rdb *redis.Client
	pg  *pgxpool.Pool
}

// WithPostgres returns the same Store with its Postgres pool set to
// pg. Used by production code paths (cmd/bot, cmd/receiver) after
// constructing the Redis-only Store via New/NewFromSecrets, and by
// tests that want to exercise history/pending methods via a real
// testcontainer-backed pool.
//
// The pool is not closed by Store; callers own its lifecycle.
func (s *Store) WithPostgres(pg *pgxpool.Pool) *Store {
	s.pg = pg
	return s
}

// Options configures the Redis connection.
type Options struct {
	Addr     string
	Password string

	// IAMAuth enables ElastiCache IAM authentication. When true, TLS is
	// required and CredentialsProvider must be set. Password is ignored.
	IAMAuth             bool
	CredentialsProvider func() (username string, password string)
}

func New(addr, password string) *Store {
	return NewWithOptions(Options{Addr: addr, Password: password})
}

// NewWithOptions creates a Store with full control over the Redis connection.
func NewWithOptions(opts Options) *Store {
	redisOpts := &redis.Options{
		Addr: opts.Addr,
	}

	if opts.IAMAuth {
		redisOpts.CredentialsProvider = opts.CredentialsProvider
		redisOpts.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	} else {
		redisOpts.Password = opts.Password
	}

	return &Store{rdb: redis.NewClient(redisOpts)}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.rdb.Ping(ctx).Err()
}

// WaitForRedis attempts to ping Redis with retries over the given timeout.
// It pings immediately, then every 5 seconds until success or timeout.
func (s *Store) WaitForRedis(ctx context.Context, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	if err := s.Ping(ctx); err == nil {
		return nil
	}

	for {
		select {
		case <-deadline:
			return fmt.Errorf("redis not available after %s", timeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.Ping(ctx); err == nil {
				return nil
			}
		}
	}
}

// Redis returns the underlying Redis client, used by components that need
// direct access (e.g. the queue worker).
func (s *Store) Redis() *redis.Client {
	return s.rdb
}

// Postgres returns the underlying connection pool. Used by the
// retention ticker and the migration tool, which need direct access
// for batched DELETEs and schema inspection.
func (s *Store) Postgres() *pgxpool.Pool {
	return s.pg
}

// ErrPendingNotFound is returned by Get/Delete/UpdateState/
// SetSlackHandle when the requested PR has no row in pending_deploys.
// Callers use this to distinguish "not our deploy" from infrastructure
// failure — the former is usually expected (late button clicks, already-
// handled PRs), the latter warrants an alert.
var ErrPendingNotFound = errors.New("pending deploy not found")

// pendingColumns lists the pending_deploys columns in the order every
// SELECT/INSERT/UPDATE in this file uses them. Kept in one place so a
// schema addition is a one-edit change.
const pendingColumns = `github_org, github_repo, pr_number, app, environment, tag,
	requester, requester_id, approver_id, pr_url, slack_channel, slack_message_ts,
	reason, requested_at, expires_at, state`

// scanPending decodes a single pgx.Row into a PendingDeploy. Shared by
// Get, GetAll, GetExpired, and GetByEnvApp.
func scanPending(row pgx.Row, d *PendingDeploy) error {
	return row.Scan(
		&d.GitHubOrg, &d.GitHubRepo, &d.PRNumber, &d.App, &d.Environment, &d.Tag,
		&d.Requester, &d.RequesterID, &d.ApproverID, &d.PRURL, &d.SlackChannel, &d.SlackMessageTS,
		&d.Reason, &d.RequestedAt, &d.ExpiresAt, &d.State,
	)
}

// Set upserts a pending deploy record. The ttl argument is preserved
// from the old Redis interface but is now computed into an expires_at
// column rather than applied as a key TTL — see note on expiration
// handling in GetExpired.
func (s *Store) Set(ctx context.Context, d *PendingDeploy, ttl time.Duration) error {
	if s.pg == nil {
		panic("store.Set: postgres pool is nil; construct Store with NewFromSecrets or WithPostgres")
	}
	// ExpiresAt on the record wins if the caller populated it
	// (matching the 1.x semantics where the pipeline set both TTL
	// and the embedded ExpiresAt field); otherwise derive from the
	// TTL argument. The column is NOT NULL, so one of the two must
	// produce a value.
	expires := d.ExpiresAt
	if expires.IsZero() {
		expires = time.Now().Add(ttl)
	}
	if d.RequestedAt.IsZero() {
		d.RequestedAt = time.Now()
	}
	state := d.State
	if state == "" {
		state = StatePending
	}

	const q = `INSERT INTO pending_deploys (` + pendingColumns + `)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
	ON CONFLICT (github_org, github_repo, pr_number) DO UPDATE SET
		app              = EXCLUDED.app,
		environment      = EXCLUDED.environment,
		tag              = EXCLUDED.tag,
		requester        = EXCLUDED.requester,
		requester_id     = EXCLUDED.requester_id,
		approver_id      = EXCLUDED.approver_id,
		pr_url           = EXCLUDED.pr_url,
		slack_channel    = EXCLUDED.slack_channel,
		slack_message_ts = EXCLUDED.slack_message_ts,
		reason           = EXCLUDED.reason,
		requested_at     = EXCLUDED.requested_at,
		expires_at       = EXCLUDED.expires_at,
		state            = EXCLUDED.state
	`
	_, err := s.pg.Exec(ctx, q,
		d.GitHubOrg, d.GitHubRepo, d.PRNumber, d.App, d.Environment, d.Tag,
		d.Requester, d.RequesterID, d.ApproverID, d.PRURL, d.SlackChannel, d.SlackMessageTS,
		d.Reason, d.RequestedAt, expires, state,
	)
	if err != nil {
		return fmt.Errorf("store pending: %w", err)
	}
	d.ExpiresAt = expires
	d.State = state
	return nil
}

// Get returns the pending deploy for the given composite primary key
// (org, repo, prNumber), or (nil, nil) if no row exists. Non-found
// is NOT surfaced as an error because the interaction handlers
// frequently do existence-checks here as the primary signal for "is
// this a live deploy?".
func (s *Store) Get(ctx context.Context, org, repo string, prNumber int) (*PendingDeploy, error) {
	if s.pg == nil {
		panic("store.Get: postgres pool is nil")
	}
	var d PendingDeploy
	const q = `SELECT ` + pendingColumns + `
		FROM pending_deploys
		WHERE github_org = $1 AND github_repo = $2 AND pr_number = $3`
	row := s.pg.QueryRow(ctx, q, org, repo, prNumber)
	if err := scanPending(row, &d); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get pending: %w", err)
	}
	return &d, nil
}

// Delete removes the pending deploy row for the given composite
// primary key. No-op and nil error if the row no longer exists.
func (s *Store) Delete(ctx context.Context, org, repo string, prNumber int) error {
	if s.pg == nil {
		panic("store.Delete: postgres pool is nil")
	}
	const q = `DELETE FROM pending_deploys
		WHERE github_org = $1 AND github_repo = $2 AND pr_number = $3`
	_, err := s.pg.Exec(ctx, q, org, repo, prNumber)
	if err != nil {
		return fmt.Errorf("delete pending: %w", err)
	}
	return nil
}

// SetSlackHandle atomically stores the (channel, message timestamp)
// pair on the pending deploy row identified by the composite primary
// key, preserving all other fields. No-op if the row no longer
// exists (the deploy was already approved/rejected/expired/cancelled
// by the time the Slack post completed). Uses a row-level UPDATE so
// a concurrent writer transitioning state is not clobbered —
// Postgres's MVCC guarantees the two updates serialize cleanly and
// neither loses.
func (s *Store) SetSlackHandle(ctx context.Context, org, repo string, prNumber int, channel, ts string) error {
	if s.pg == nil {
		panic("store.SetSlackHandle: postgres pool is nil")
	}
	const q = `UPDATE pending_deploys
	SET slack_channel = $4, slack_message_ts = $5
	WHERE github_org = $1 AND github_repo = $2 AND pr_number = $3`
	_, err := s.pg.Exec(ctx, q, org, repo, prNumber, channel, ts)
	if err != nil {
		return fmt.Errorf("set slack handle: %w", err)
	}
	// No error if zero rows affected — "record gone" is not an error
	// here, same as the 1.x Redis behaviour.
	return nil
}

// UpdateState transitions the pending deploy's state column. Returns
// ErrPendingNotFound if the row doesn't exist, so callers can
// distinguish "nothing to update" from infrastructure failure.
func (s *Store) UpdateState(ctx context.Context, org, repo string, prNumber int, state string) error {
	if s.pg == nil {
		panic("store.UpdateState: postgres pool is nil")
	}
	const q = `UPDATE pending_deploys SET state = $4
		WHERE github_org = $1 AND github_repo = $2 AND pr_number = $3`
	tag, err := s.pg.Exec(ctx, q, org, repo, prNumber, state)
	if err != nil {
		return fmt.Errorf("update state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("deploy %d: %w", prNumber, ErrPendingNotFound)
	}
	return nil
}

// GetAll returns every pending row, regardless of state. Order is
// not guaranteed; callers that want newest-first order by requested_at
// on their own.
//
// The 1.x-compatible semantics say "every pending record" — which in
// Redis meant "every `pending:*` key, some of which may be in
// StateMerging or StateMerged." Postgres preserves that: we return
// every row without filtering on state, and callers that only want
// in-flight ones apply their own state==pending filter.
func (s *Store) GetAll(ctx context.Context) ([]*PendingDeploy, error) {
	if s.pg == nil {
		panic("store.GetAll: postgres pool is nil")
	}
	const q = `SELECT ` + pendingColumns + ` FROM pending_deploys`
	rows, err := s.pg.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list pending: %w", err)
	}
	defer rows.Close()

	var out []*PendingDeploy
	for rows.Next() {
		d := &PendingDeploy{}
		if err := scanPending(rows, d); err != nil {
			return nil, fmt.Errorf("scan pending: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending: %w", err)
	}
	return out, nil
}

// GetExpired returns every pending row (state='pending') whose
// expires_at is in the past. Used by the sweeper. The partial index
// `pending_deploys_expires_idx` on (expires_at) WHERE state='pending'
// makes this a cheap indexed range scan rather than a sequential
// full-table scan.
func (s *Store) GetExpired(ctx context.Context) ([]*PendingDeploy, error) {
	if s.pg == nil {
		panic("store.GetExpired: postgres pool is nil")
	}
	const q = `SELECT ` + pendingColumns + `
	FROM pending_deploys
	WHERE state = 'pending' AND expires_at <= NOW()
	ORDER BY expires_at`
	rows, err := s.pg.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list expired: %w", err)
	}
	defer rows.Close()

	var out []*PendingDeploy
	for rows.Next() {
		d := &PendingDeploy{}
		if err := scanPending(rows, d); err != nil {
			return nil, fmt.Errorf("scan expired: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired: %w", err)
	}
	return out, nil
}

// GetByEnvApp returns the first in-flight deploy for the given
// environment and app, or (nil, nil) if none exists. Used to surface
// the existing PR link when a lock is contested on a new modal
// submit. Only considers rows with state='pending' — merging/merged
// rows are mid-completion and not user-visible as "another deploy is
// in flight."
func (s *Store) GetByEnvApp(ctx context.Context, env, app string) (*PendingDeploy, error) {
	if s.pg == nil {
		panic("store.GetByEnvApp: postgres pool is nil")
	}
	const q = `SELECT ` + pendingColumns + `
	FROM pending_deploys
	WHERE state = 'pending' AND environment = $1 AND app = $2
	ORDER BY requested_at DESC
	LIMIT 1`
	var d PendingDeploy
	row := s.pg.QueryRow(ctx, q, env, app)
	if err := scanPending(row, &d); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get by env/app: %w", err)
	}
	return &d, nil
}

// historyColumns lists the history columns in the fixed order every
// SELECT/INSERT in this file uses. Kept in one place for the same
// reason as pendingColumns.
const historyColumns = `github_org, github_repo, event_type, app, environment, tag,
	pr_number, pr_url, requester_id, approver_id, completed_at,
	gitops_commit_sha, slack_channel, slack_message_ts`

// scanHistory decodes a single pgx.Row into a HistoryEntry. pr_number,
// pr_url, approver_id, gitops_commit_sha, slack_channel, and
// slack_message_ts are nullable in the schema, so they're scanned
// through sql.Null* intermediates and transcribed on success.
func scanHistory(row pgx.Row, e *HistoryEntry) error {
	var (
		org, repo, prURL, approverID, sha, slackCh, slackTS *string
		prNumber                                            *int32
	)
	if err := row.Scan(
		&org, &repo, &e.EventType, &e.App, &e.Environment, &e.Tag,
		&prNumber, &prURL, &e.RequesterID, &approverID, &e.CompletedAt,
		&sha, &slackCh, &slackTS,
	); err != nil {
		return err
	}
	if org != nil {
		e.GitHubOrg = *org
	}
	if repo != nil {
		e.GitHubRepo = *repo
	}
	if prNumber != nil {
		e.PRNumber = int(*prNumber)
	}
	if prURL != nil {
		e.PRURL = *prURL
	}
	if approverID != nil {
		e.ApproverID = *approverID
	}
	if sha != nil {
		e.GitopsCommitSHA = *sha
	}
	if slackCh != nil {
		e.SlackChannel = *slackCh
	}
	if slackTS != nil {
		e.SlackMessageTS = *slackTS
	}
	return nil
}

// PushHistory inserts a HistoryEntry into the history table. Unlike
// the 1.x Redis implementation, there's no 100-entry cap — retention
// is governed by the retention ticker per the configured
// history_retention duration (default 2 years, floor 390 days).
func (s *Store) PushHistory(ctx context.Context, e HistoryEntry) error {
	if s.pg == nil {
		panic("store.PushHistory: postgres pool is nil")
	}
	const q = `INSERT INTO history (` + historyColumns + `)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`
	_, err := s.pg.Exec(ctx, q,
		nullIfEmpty(e.GitHubOrg), nullIfEmpty(e.GitHubRepo),
		e.EventType, e.App, e.Environment, e.Tag,
		nullIfZero(e.PRNumber), nullIfEmpty(e.PRURL),
		e.RequesterID, nullIfEmpty(e.ApproverID),
		e.CompletedAt,
		nullIfEmpty(e.GitopsCommitSHA),
		nullIfEmpty(e.SlackChannel), nullIfEmpty(e.SlackMessageTS),
	)
	if err != nil {
		return fmt.Errorf("push history: %w", err)
	}
	return nil
}

// GetHistory returns up to limit rows from history, newest first.
// When appFilter is non-empty, only rows matching that app (the
// composite FullName, e.g. "myapp-dev") are returned — backed by
// history_app_env_completed_idx. When empty, all rows are returned
// backed by history_completed_at_idx.
// limit <= 0 falls back to DefaultHistoryLimit.
func (s *Store) GetHistory(ctx context.Context, appFilter string, limit int) ([]HistoryEntry, error) {
	if s.pg == nil {
		panic("store.GetHistory: postgres pool is nil")
	}
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}

	var (
		q    string
		args []interface{}
	)
	if appFilter != "" {
		q = `SELECT ` + historyColumns + `
		FROM history
		WHERE app = $1
		ORDER BY completed_at DESC
		LIMIT $2`
		args = []interface{}{appFilter, limit}
	} else {
		q = `SELECT ` + historyColumns + `
		FROM history
		ORDER BY completed_at DESC
		LIMIT $1`
		args = []interface{}{limit}
	}

	rows, err := s.pg.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("get history: %w", err)
	}
	defer rows.Close()

	out := make([]HistoryEntry, 0, limit)
	for rows.Next() {
		var e HistoryEntry
		if err := scanHistory(rows, &e); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate history: %w", err)
	}
	return out, nil
}

// FindHistoryBySHA returns the single most-recent history entry whose
// gitops_commit_sha matches sha, or (nil, nil) if none matches.
// Backed by the partial index history_gitops_sha_idx WHERE NOT NULL,
// so this is an O(1) lookup instead of the O(N) list scan it was in
// 1.x. Used by the ArgoCD notification handler to correlate a synced
// revision back to the deploy that produced it.
//
// Returns (nil, nil) when sha is empty — callers feed this straight
// from upstream payloads where the field may be absent, and a missing
// SHA is "not matched," not infrastructure failure.
func (s *Store) FindHistoryBySHA(ctx context.Context, sha string) (*HistoryEntry, error) {
	if sha == "" {
		return nil, nil
	}
	if s.pg == nil {
		panic("store.FindHistoryBySHA: postgres pool is nil")
	}
	const q = `SELECT ` + historyColumns + `
	FROM history
	WHERE gitops_commit_sha = $1
	ORDER BY completed_at DESC
	LIMIT 1`
	var e HistoryEntry
	row := s.pg.QueryRow(ctx, q, sha)
	if err := scanHistory(row, &e); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find history by sha: %w", err)
	}
	return &e, nil
}

// nullIfEmpty turns an empty string into a nil *string so Postgres
// receives SQL NULL rather than ” for nullable text columns. This
// matters for indexes with WHERE NOT NULL clauses (the gitops_sha
// partial index in particular) — an empty string would be a distinct
// non-null value and pollute the index.
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nullIfZero turns a zero int into a nil *int32 for the same reason.
// Applies to pr_number on history rows where direct-deploy paths may
// not carry a PR.
func nullIfZero(n int) *int32 {
	if n == 0 {
		return nil
	}
	v := int32(n)
	return &v
}

// deployLockKey returns the Redis key for a per-app deploy lock scoped to its
// environment, preventing false conflicts between same-named apps in different
// environments.
func deployLockKey(env, app string) string {
	return lockPrefix + env + "/" + app
}

// AcquireLock attempts to claim the per-app deploy lock. It returns true if
// the lock was acquired, false if another deploy is already in progress.
// holder is stored as the lock value (use the requester's Slack user ID so
// "who holds this?" is answerable by inspecting Redis directly).
func (s *Store) AcquireLock(ctx context.Context, env, app, holder string, ttl time.Duration) (bool, error) {
	err := s.rdb.SetArgs(ctx, deployLockKey(env, app), holder, redis.SetArgs{
		Mode: "NX",
		TTL:  ttl,
	}).Err()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("acquire lock %s/%s: %w", env, app, err)
	}
	return true, nil
}

// IsLocked returns true if a deploy lock is currently held for the given app.
func (s *Store) IsLocked(ctx context.Context, env, app string) (bool, error) {
	n, err := s.rdb.Exists(ctx, deployLockKey(env, app)).Result()
	if err != nil {
		return false, fmt.Errorf("check lock %s/%s: %w", env, app, err)
	}
	return n > 0, nil
}

// ReleaseLock deletes the per-app deploy lock.
func (s *Store) ReleaseLock(ctx context.Context, env, app string) error {
	return s.rdb.Del(ctx, deployLockKey(env, app)).Err()
}

// TryLock acquires a named system lock (distinct from per-app deploy locks).
// Returns true if the lock was acquired. Used for single-instance coordination
// of background processes (e.g. the sweeper) without leader election.
func (s *Store) TryLock(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	err := s.rdb.SetArgs(ctx, sysLockPrefix+name, "1", redis.SetArgs{
		Mode: "NX",
		TTL:  ttl,
	}).Err()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("trylock %s: %w", name, err)
	}
	return true, nil
}

// GetThreadTS returns the Slack thread timestamp for an environment's deploy
// thread, or empty string if none exists.
func (s *Store) GetThreadTS(ctx context.Context, env string) (string, error) {
	ts, err := s.rdb.Get(ctx, threadPrefix+env).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get thread ts %s: %w", env, err)
	}
	return ts, nil
}

// SetThreadTS atomically claims the thread slot for an environment. Uses SET NX
// so only the first caller wins. Returns true if the value was set.
func (s *Store) SetThreadTS(ctx context.Context, env, ts string, ttl time.Duration) (bool, error) {
	err := s.rdb.SetArgs(ctx, threadPrefix+env, ts, redis.SetArgs{
		Mode: "NX",
		TTL:  ttl,
	}).Err()
	if err == redis.Nil {
		return false, nil
	}
	return err == nil, err
}

// UpdateThreadTS overwrites the thread timestamp (e.g. replacing a "pending"
// placeholder with the real Slack message timestamp).
func (s *Store) UpdateThreadTS(ctx context.Context, env, ts string, ttl time.Duration) error {
	return s.rdb.Set(ctx, threadPrefix+env, ts, ttl).Err()
}

// DeleteThreadTS removes the thread timestamp for an environment.
func (s *Store) DeleteThreadTS(ctx context.Context, env string) error {
	return s.rdb.Del(ctx, threadPrefix+env).Err()
}
