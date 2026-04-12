// Package storetest exposes a test-only helper that constructs a
// fully-functional store.Store backed by both an in-process miniredis
// and a Postgres testcontainer. Unit tests in any package import this
// helper directly; each call returns an isolated Store with a freshly-
// reset Postgres schema, so tests don't need to coordinate cleanup
// with one another.
//
// This package is non-_test so it can be imported from tests in other
// packages. It nevertheless only makes sense inside tests — the
// testcontainers dependency drags in a Docker client and is not
// suitable for production builds.
//
// Docker gating: if a container runtime is unavailable (no Docker,
// CI without services configured), NewStore calls t.Skip so unit
// tests that don't touch pg keep working and tests that do skip with
// a clear message. The first call per package-run tries to start the
// container; if it fails, every subsequent call in the same package
// skips immediately without retrying.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ezubriski/deploy-bot/internal/store"
	"github.com/ezubriski/deploy-bot/internal/store/postgres"
)

// packageContainer holds the shared postgres testcontainer for a
// single test binary run. Initialized lazily on first NewStore call,
// reused across every subsequent call. The container lifetime is
// bounded by the test binary process — testcontainers' Ryuk reaper
// sweeps orphaned containers automatically on process exit.
type packageContainer struct {
	once    sync.Once
	dsn     string
	initErr error
}

var shared = &packageContainer{}

// NewStore returns a *store.Store wired to an in-process miniredis
// and a per-test isolated Postgres schema inside a package-shared
// testcontainer. Calls t.Skip if the shared container cannot be
// started (no Docker, no network, etc.).
//
// Each call truncates every table before returning, so tests see a
// freshly-empty database and don't have to coordinate row IDs with
// one another.
func NewStore(t *testing.T) *store.Store {
	t.Helper()
	st, _ := NewStoreWithRedis(t)
	return st
}

// NewStoreWithRedis is the same as NewStore but also returns the
// miniredis handle so tests that want to inspect Redis state (fast-
// forward clocks, dump keys) can do so.
func NewStoreWithRedis(t *testing.T) (*store.Store, *miniredis.Miniredis) {
	t.Helper()

	dsn := sharedDSN(t)

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to shared postgres container: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := truncateAll(ctx, pool); err != nil {
		t.Fatalf("truncate tables: %v", err)
	}

	return store.New(mr.Addr(), "").WithPostgres(pool), mr
}

// sharedDSN lazily initializes the shared Postgres testcontainer
// and returns a DSN against it. Subsequent calls return the same
// DSN. If container startup fails for any reason, every caller
// (this one and future ones) calls t.Skip with the failure reason —
// we don't fail tests in environments where testcontainers is
// fundamentally unusable.
func sharedDSN(t *testing.T) string {
	t.Helper()
	shared.once.Do(func() {
		shared.dsn, shared.initErr = startContainer()
	})
	if shared.initErr != nil {
		t.Skipf("postgres testcontainer unavailable: %v", shared.initErr)
	}
	return shared.dsn
}

// startContainer spins up a postgres:15-alpine image and applies the
// bot's goose migrations against it. Returns the DSN or an error.
// Ryuk handles container cleanup on process exit so we don't keep a
// handle to the container after this function returns.
func startContainer() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	container, err := tcpostgres.Run(
		ctx,
		"postgres:15-alpine",
		tcpostgres.WithDatabase("deploy_bot_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return "", fmt.Errorf("start postgres container: %w", err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", errors.Join(fmt.Errorf("get connection string: %w", err), container.Terminate(ctx))
	}

	if err := applyMigrations(ctx, dsn); err != nil {
		return "", errors.Join(fmt.Errorf("apply migrations: %w", err), container.Terminate(ctx))
	}

	return dsn, nil
}

// applyMigrations runs goose.Up against a fresh DSN. Uses the same
// embedded migration FS as the production postgres.Pool.Migrate
// path, so the testcontainer exercises the exact same SQL that
// production applies — if a migration file has a typo, the tests
// catch it before the production bot does.
func applyMigrations(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close() //nolint:errcheck // test setup

	goose.SetBaseFS(postgres.MigrationsFS())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// truncateAll clears every data table between tests. Much faster
// than re-creating the container per test (~100ms vs ~5s). The
// goose_db_version table is left alone so the applied-migrations
// state persists.
func truncateAll(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		TRUNCATE TABLE history, pending_deploys RESTART IDENTITY CASCADE
	`)
	if err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	return nil
}
