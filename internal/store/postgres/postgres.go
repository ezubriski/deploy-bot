// Package postgres implements the durable-state backing store for
// deploy-bot 3.0. It holds two tables — `history` (completed deploy
// events) and `pending_deploys` (in-flight deploys awaiting approval,
// merge, or expiration) — and provides the connection pool,
// authentication, and migration invocation that other code in the
// bot uses via store.Store.
//
// Everything that isn't history or pending-deploys lives in Redis and
// is unchanged by 2.0:
//
//   - Streams (user/ecr/argocd events): Redis Streams, consumer
//     groups, XAUTOCLAIM.
//   - Locks (per-app deploy lock, sweeper/reconcile leader locks):
//     Redis SET NX EX.
//   - Dedupe markers (argocd, ecr): Redis SET NX EX with TTL.
//   - Caches (identity, approver-team membership, ECR tags): Redis
//     HASH/SET.
//   - Thread timestamps: Redis STRING.
//
// The split is described in docs/design/postgres-migration.md: Postgres
// for slow-moving durable structured state, Redis for fast-moving
// ephemeral coordination. Each tool does what it's good at.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
)

// migrationsFS embeds the goose SQL migration files so the bot binary
// is self-contained (operators don't have to ship a separate
// migrations directory alongside the image).
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// MigrationsFS returns the embedded migrations filesystem for reuse
// by test helpers (internal/storetest) that want to apply the same
// schema to a testcontainer without going through the full Pool
// construction path. Returning embed.FS by value is fine — it's a
// slim wrapper around a *embed.filedata pointer and immutable.
func MigrationsFS() embed.FS {
	return migrationsFS
}

// migrationAdvisoryLockID is the key passed to pg_advisory_lock() in
// Migrate() to serialize concurrent migration attempts from multiple
// replicas during a rolling restart. Value is a fixed large constant
// chosen to be unlikely to collide with any other code that might
// also use advisory locks against the same database — CRC32 of the
// literal "deploybot.migrations.v1" folded into the high word, with
// a recognizable low word so manual `pg_locks` inspection surfaces a
// human-legible number.
//
// The lock is session-scoped (not transaction-scoped), so if a
// replica crashes mid-migration the server releases it automatically
// when the connection drops. No stale-lock recovery logic needed.
const migrationAdvisoryLockID int64 = 0x6465706c6f79626f // "deploybo" ASCII

// DefaultWaitTimeout is the upper bound on how long New() will wait
// for Postgres to become reachable before giving up at startup.
const DefaultWaitTimeout = 60 * time.Second

// Pool wraps a pgxpool.Pool along with the bits of configuration
// needed for migration invocation and graceful shutdown. It's
// constructed once per process (once in the bot, once in the
// receiver) and handed to store.Store.
type Pool struct {
	Pool *pgxpool.Pool

	cfg config.PostgresConfig
	log *zap.Logger
}

// New constructs a Pool from the loaded PostgresConfig and Secrets.
// The connection pool is initialized and a single ping is issued
// before returning; callers should still follow up with WaitFor() if
// they want retries on transient startup unavailability.
//
// Authentication mode is selected by secrets.PostgresIAMAuth:
//
//   - false (default): static password. secrets.PostgresPassword is
//     used directly. The DSN embeds it, same as any conventional
//     Postgres client.
//
//   - true: AWS RDS IAM auth. A short-lived (~15m) token is generated
//     via rds/auth.BuildAuthToken() and refreshed by a BeforeConnect
//     hook on the pool, mirroring the existing ElastiCache IAM auth
//     pattern in store.NewFromSecrets. secrets.PostgresRDSRegion
//     must be set; Validate() enforces this.
func New(ctx context.Context, cfg config.PostgresConfig, secrets *config.Secrets, log *zap.Logger) (*Pool, error) {
	if log == nil {
		log = zap.NewNop()
	}

	poolCfg, err := buildPoolConfig(cfg, secrets)
	if err != nil {
		return nil, fmt.Errorf("build postgres pool config: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}

	p := &Pool{
		Pool: pool,
		cfg:  cfg,
		log:  log,
	}
	return p, nil
}

// Ping verifies the pool can reach Postgres with a round-trip query.
// Uses a short per-query timeout so a hung Postgres doesn't wedge
// startup or health checks.
func (p *Pool) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return p.Pool.Ping(ctx)
}

// WaitFor retries Ping until success or the timeout elapses. Mirrors
// store.Store.WaitForRedis so the bot/receiver startup paths have
// identical shape across the two data stores.
func (p *Pool) WaitFor(ctx context.Context, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	if err := p.Ping(ctx); err == nil {
		return nil
	}

	for {
		select {
		case <-deadline:
			return fmt.Errorf("postgres not available after %s", timeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := p.Ping(ctx); err == nil {
				return nil
			}
		}
	}
}

// Migrate runs goose.Up() against the embedded migrations, gated by a
// Postgres advisory lock so only one replica wins during a rolling
// restart. Does nothing when AutoMigrate is false — operators flip
// the flag on for the duration of an upgrade window that ships a new
// migration, observe the result, then flip it back off.
//
// Should be called from the bot binary only. The receiver never runs
// migrations; that's an explicit invariant in cmd/receiver/main.go.
func (p *Pool) Migrate(ctx context.Context) error {
	if !p.cfg.AutoMigrate {
		p.log.Info("postgres: auto_migrate is false, skipping goose.Up",
			zap.String("hint", "set postgres.auto_migrate: true in config for the duration of an upgrade window to apply new migrations"),
		)
		return nil
	}

	p.log.Info("postgres: acquiring migration advisory lock",
		zap.Int64("lock_id", migrationAdvisoryLockID),
	)

	// Take the advisory lock on a dedicated connection so goose's
	// own connection pool behaviour can't interfere with the lock's
	// session scope. Holds until Release at the end; goose runs
	// between the two.
	conn, err := p.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("postgres: acquire conn for migration lock: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationAdvisoryLockID); err != nil {
		return fmt.Errorf("postgres: pg_advisory_lock: %w", err)
	}
	defer func() {
		// pg_advisory_unlock returns boolean; ignore the value but
		// not the error. If this fails we still release on
		// connection close, so it's log-and-continue.
		if _, unlockErr := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationAdvisoryLockID); unlockErr != nil {
			p.log.Warn("postgres: pg_advisory_unlock failed; connection teardown will release",
				zap.Error(unlockErr),
			)
		}
	}()

	// Goose needs a *sql.DB, not a pgx pool. Open a temporary
	// database/sql handle over the same pgx driver (via stdlib
	// compatibility shim) so goose can use its own sql.Tx machinery
	// without fighting the pgx pool. We close it as soon as goose
	// finishes; the real pool is untouched.
	sqlDB := stdlib.OpenDBFromPool(p.Pool)
	defer func() {
		if closeErr := sqlDB.Close(); closeErr != nil {
			p.log.Warn("postgres: close goose sql.DB handle", zap.Error(closeErr))
		}
	}()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("postgres: goose set dialect: %w", err)
	}
	goose.SetLogger(gooseZapAdapter{log: p.log})

	start := time.Now()
	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("postgres: goose up: %w", err)
	}

	p.log.Info("postgres: migrations applied",
		zap.Duration("duration", time.Since(start)),
	)
	return nil
}

// Close drains the pool. Idempotent; safe to call on shutdown paths
// that may also be invoked from a defer.
func (p *Pool) Close() {
	if p == nil || p.Pool == nil {
		return
	}
	p.Pool.Close()
}

// buildPoolConfig translates a PostgresConfig + Secrets into the
// pgxpool.Config that pgxpool.NewWithConfig consumes. Shared by both
// auth modes — the IAM branch just installs a BeforeConnect hook to
// refresh the password on each new connection.
//
// The pool config is seeded by parsing a *minimal* DSN that carries
// only `host` and `sslmode`. pgx needs both in the DSN to build the
// TLS config correctly (ServerName for verify-full is derived from
// the host during ParseConfig). Everything else — port, user,
// database, password — is assigned programmatically on ConnConfig
// so values with spaces, single quotes, or backslashes can't corrupt
// the libpq keyword/value grammar. Passwords in particular very
// commonly contain special characters, and the keyword/value format
// requires them to be single-quoted-and-escaped, which is fragile
// and easy to get wrong. Assigning the field directly sidesteps the
// whole category.
func buildPoolConfig(cfg config.PostgresConfig, secrets *config.Secrets) (*pgxpool.Config, error) {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		return nil, errors.New("postgres.host is empty (should have been caught by Validate)")
	}

	// Host is DNS-safe (ASCII, no spaces); sslmode is one of a small
	// ASCII allowlist — both are safe to embed unquoted. pgx's
	// ParseConfig rejects unknown sslmode values so an unexpected
	// value surfaces here, not silently downstream.
	dsn := fmt.Sprintf("host=%s sslmode=%s", host, cfg.SSLModeValue())
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	poolCfg.ConnConfig.Port = uint16(cfg.PortValue())
	poolCfg.ConnConfig.Database = cfg.Database
	poolCfg.ConnConfig.User = cfg.User
	if !secrets.PostgresIAMAuth {
		poolCfg.ConnConfig.Password = secrets.PostgresPassword
	}

	if secrets.PostgresIAMAuth {
		if err := installIAMAuthHook(poolCfg, cfg, secrets); err != nil {
			return nil, fmt.Errorf("install iam auth hook: %w", err)
		}
	}

	return poolCfg, nil
}

// gooseZapAdapter adapts zap.Logger to the goose.Logger interface so
// migration output goes through the same structured logger as the
// rest of the bot.
type gooseZapAdapter struct {
	log *zap.Logger
}

func (g gooseZapAdapter) Fatalf(format string, v ...interface{}) {
	g.log.Fatal(fmt.Sprintf(format, v...))
}

func (g gooseZapAdapter) Printf(format string, v ...interface{}) {
	// Trim trailing newline — goose adds one and zap does its own.
	msg := strings.TrimRight(fmt.Sprintf(format, v...), "\n")
	g.log.Info("goose: " + msg)
}
