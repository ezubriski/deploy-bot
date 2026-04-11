# Design: Postgres for durable state (deploy-bot 2.0)

**Status:** draft, pre-implementation
**Target release:** 2.0 (hard cut, major version bump)
**Author context:** follow-up to a live discussion about where durable state
belongs. Informed by the actual failure modes observed during phase-4
smoke testing on the homelab.

## Goals

1. **Durable storage for state that's painful to lose.** Specifically deploy
   history and in-flight pending deploys. Losing either today silently
   degrades user-facing behaviour in ways that are hard to notice until an
   incident happens.
2. **Remove the 100-entry history cap.** This is a Redis LIST artifact, not
   a retention policy. It silently causes `/deploy rollback` and ArgoCD
   notification correlation to fall off the cliff during busy periods.
3. **Let Redis stop being durable.** Once durable state is in Postgres,
   Redis can be run as a pure cache + queue + lock store — no AOF, no RDB,
   ephemeral-disk-is-fine. That's a net operational simplification, not an
   additional burden, even though we're adding a dependency.
4. **Simpler persistence code.** One place for durable state, not two,
   means no dual-write branching, no "which store is authoritative"
   decisions in the read path, and no drift risk between equivalent paths.

## Non-goals

1. **Running deploy-bot without Redis.** Redis stays required — it's the
   right tool for streams, locks, dedupe markers, identity/approver caches,
   and thread-timestamp coordination. "Degraded mode without Redis" was
   explicitly ruled out as an objective; if Redis is down, the bot is down,
   and that's fine because it's a hard failure, not a silent one.
2. **Moving audit logging to Postgres.** The audit path already writes to
   S3 (or zap fallback) for compliance-grade durability. Adding a second
   target would be churn without benefit.
3. **Moving Redis Streams to Postgres-as-queue.** `XREADGROUP` + consumer
   groups + `XAUTOCLAIM` is exactly the tool for at-least-once event
   processing; Postgres is functional here via `SELECT FOR UPDATE SKIP
   LOCKED` but it's operationally worse (higher latency, no consumer
   groups, awkward backpressure). Streams stay in Redis; Redis stays
   required.
4. **Event sourcing / append-only log of every state transition.**
   Interesting but out of scope for "fix the durability pain." The
   table-per-concern model described below is enough for every concrete
   use case we have.
5. **Backwards compatibility with 1.x config files without intervention.**
   This is a major version bump; operators will edit `config.json` and run
   a migration script as part of the upgrade.

## Current state summary

Redis holds everything today. Pain rankings from the
[discussion](../../README.md):

| Category | Key / structure | Pain if lost |
|---|---|---|
| Durable structured state | `history` list, `pending:<pr>` hashes | **High** |
| Event queues | `user:events`, `ecr:events`, `argocd:events` (streams) | **High** (stays in Redis) |
| Locks | `lock:<env>/<app>`, sweeper lock, reconcile lock | Low |
| Short-TTL coordination | `thread:<env>`, dedupe markers | Low |
| Caches | approvers, identity, ECR tags | Low |

This doc is about moving the first row to Postgres. Everything else stays
in Redis.

## What moves

### `history` → `history` table

The `history` LIST in Redis becomes a Postgres table. Every completed
deploy event (approved, rejected, expired, cancelled) is inserted here by
`handleApprove` / `handleReject` / the sweeper / the cancel handler.

Read sites today and what they become:

| Current call | Current cost | New implementation |
|---|---|---|
| `store.GetHistory(ctx, limit)` | `LRANGE + decode` | `SELECT ... ORDER BY completed_at DESC LIMIT $1` |
| `store.FindHistoryBySHA(ctx, sha)` | `LRANGE + linear scan in Go` | `SELECT ... WHERE gitops_commit_sha = $1 LIMIT 1`, indexed |
| Rollback target lookup (phase 4) | `LRANGE + linear scan in Go` | `SELECT tag FROM history WHERE app = $1 AND environment = $2 AND event_type = 'approved' AND completed_at < $3 ORDER BY completed_at DESC LIMIT 1`, indexed |
| `/deploy history [app-env]` | `LRANGE + filter in Go` | `SELECT ... WHERE app = $1 [AND environment = $2] ORDER BY completed_at DESC LIMIT $n` |

All of these get faster. FindHistoryBySHA in particular goes from `O(N)`
over the in-memory list to `O(1)` via an index, which matters because it
runs on every single ArgoCD notification.

### `pending:<pr>` → `pending_deploys` table

Each in-flight deploy waiting for approval becomes a row. The table has
an `expires_at` column equivalent to today's Redis TTL, and the sweeper
query becomes:

```sql
SELECT * FROM pending_deploys
WHERE state = 'pending' AND expires_at <= NOW()
FOR UPDATE SKIP LOCKED;
```

The sweeper's current "reconcile from GitHub" path — which re-hydrates
orphaned PRs when Redis loses state — stays as a sanity check but becomes
a rarely-taken code path rather than the primary recovery mechanism. Its
job transitions from "make sure we didn't drop a deploy" to "make sure
Postgres and GitHub agree on what's open."

## Schema

### `history`

```sql
CREATE TABLE history (
    id                  BIGSERIAL    PRIMARY KEY,
    event_type          TEXT         NOT NULL CHECK (event_type IN (
                                        'approved', 'rejected', 'expired', 'cancelled'
                                    )),
    app                 TEXT         NOT NULL,  -- composite FullName, e.g. "nginx-01-dev"
    environment         TEXT         NOT NULL,
    tag                 TEXT         NOT NULL,
    pr_number           INTEGER,                 -- NULL for direct-deploy paths if any
    pr_url              TEXT,
    requester_id        TEXT         NOT NULL,
    approver_id         TEXT,                    -- NULL for non-approved events
    completed_at        TIMESTAMPTZ  NOT NULL,
    gitops_commit_sha   TEXT,                    -- NULL for rejected/expired/cancelled
    slack_channel       TEXT,
    slack_message_ts    TEXT,
    rejection_reason    TEXT,
    inserted_at         TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Rollback target lookup, /deploy history filtering.
CREATE INDEX history_app_env_completed_idx
    ON history (app, environment, completed_at DESC);

-- ArgoCD notification correlation. Partial index: only rows that
-- actually carry a merge SHA are candidates, which excludes
-- rejected/expired/cancelled rows and keeps the index small.
CREATE INDEX history_gitops_sha_idx
    ON history (gitops_commit_sha)
    WHERE gitops_commit_sha IS NOT NULL;

-- Retention job predicate. Unconditional index on completed_at is
-- cheap and also serves the "newest first" fallback query that
-- doesn't filter by app.
CREATE INDEX history_completed_at_idx
    ON history (completed_at);
```

### `pending_deploys`

```sql
CREATE TABLE pending_deploys (
    pr_number         INTEGER      PRIMARY KEY,
    app               TEXT         NOT NULL,
    environment       TEXT         NOT NULL,
    tag               TEXT         NOT NULL,
    requester_id      TEXT         NOT NULL,
    approver_id       TEXT,                      -- may be empty until reassignment
    pr_url            TEXT         NOT NULL,
    slack_channel     TEXT,
    slack_message_ts  TEXT,
    reason            TEXT,                      -- free-form reason from the modal
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ  NOT NULL,
    state             TEXT         NOT NULL DEFAULT 'pending' CHECK (state IN (
                                     'pending', 'approved', 'rejected', 'expired', 'cancelled'
                                  ))
);

-- Sweeper expiration scan. Partial index keeps it tight since terminal-
-- state rows are uninteresting.
CREATE INDEX pending_deploys_expires_idx
    ON pending_deploys (expires_at)
    WHERE state = 'pending';

-- Interactive "show pending for this app" queries.
CREATE INDEX pending_deploys_app_env_idx
    ON pending_deploys (app, environment)
    WHERE state = 'pending';
```

Terminal-state rows (approved/rejected/expired/cancelled) stay in this
table briefly for idempotency (so a late button-click on an already-closed
PR gets a clean error rather than a 404 on the row lookup) and then age
out via a separate retention job — see [Retention](#retention).

## Retention

Hard-coded safeguard floor: **390 days**. The rationale, verbatim from
the decision discussion: "audits don't happen immediately a period end,"
so the minimum must tolerate the gap between when a compliance period
closes and when auditors get around to inspecting the data.

```go
// internal/store/postgres/retention.go
const minRetentionDays = 390

type RetentionConfig struct {
    HistoryRetention          time.Duration // default: 2 * 365 * 24h
    PendingTerminalRetention  time.Duration // default: 30 * 24h (see below)
}

func (c RetentionConfig) Validate() error {
    if c.HistoryRetention < minRetentionDays*24*time.Hour {
        return fmt.Errorf("history_retention must be >= %d days for audit compliance, got %s",
            minRetentionDays, c.HistoryRetention)
    }
    // PendingTerminalRetention is not compliance-bound; a short floor of
    // 1h is sufficient to cover the idempotency window.
    if c.PendingTerminalRetention < time.Hour {
        return fmt.Errorf("pending_terminal_retention must be >= 1h, got %s",
            c.PendingTerminalRetention)
    }
    return nil
}
```

`Validate()` is called at config load; a violation is a fatal startup
error with a clear message pointing at the config key.

**Retention is a background ticker, not a slash command.** A
`retention.Retainer` type similar to the existing `sweeper.Sweeper`:

- Runs on a slow ticker (default: once per 24h).
- Before deleting, logs a pre-flight `SELECT COUNT(*) ... WHERE
  completed_at < NOW() - $1` with the breakdown by `event_type`. This
  lets a misconfig or bug be visible in logs before it's destructive.
- Deletes in batches (`DELETE FROM history WHERE id IN (SELECT id FROM
  history WHERE completed_at < $1 LIMIT 1000)`) to avoid long-running
  transactions on a large table. Loops until `DELETE` affects 0 rows.
- Gated by a Redis lock (same pattern as sweeper) so only one worker
  pod runs it at a time.
- Emits a metric `deploybot_retention_rows_deleted_total{table, event_type}`.

`pending_deploys` terminal rows get their own pass: delete rows where
`state != 'pending' AND created_at < NOW() - pending_terminal_retention`.
The `1h` floor is enough to cover the late-button-click idempotency
window without keeping terminal rows around longer than useful.

**No retention slash command.** `/deploy` stays a user-facing command;
retention is an ops concern. If an operator needs to run it manually
(e.g., after a misconfig temporarily prevented scheduled runs), the
`deploy-bot-config` CLI grows a `retention dry-run` / `retention run`
subcommand that reads the same config and talks to the same Postgres.

### Hard-delete vs soft-delete

Deploy-bot will **hard-delete**. Rationale:

- The `minRetentionDays = 390` floor is enforced at config load, so a
  misconfig cannot silently delete fresh data — startup fails loudly.
- The retention job logs a pre-flight count before deleting, so a bug
  in the predicate is visible in logs before it's destructive.
- The read sites are confined to `internal/store/postgres/history.go`
  (a handful of methods), so the "forget the `WHERE deleted_at IS NULL`
  filter" footgun that makes soft-delete risky in large codebases is
  small here.
- Postgres backups (whatever the operator already runs — `pg_dump`, WAL
  archiving, snapshot-based) cover the "oops, wrong retention" recovery
  case. If deploy-bot is run against a Postgres with no backups, that's
  an operator choice whose consequences are predictable.

If a future incident reveals that the "hard-delete + floor + pre-flight"
model isn't enough — say, a bug delivered unusually destructive retention
semantics — the upgrade to soft-delete is ~30 lines of wiring and one
partial-index migration. Not a one-way door.

## Store interface

Today `internal/store/store.go` wraps a single `*redis.Client`. After
this change it wraps a `*redis.Client` **and** a `*pgxpool.Pool`:

```go
// internal/store/store.go
type Store struct {
    redis *redis.Client
    pg    *pgxpool.Pool
    clock clock.Clock  // existing
    log   *zap.Logger
}
```

Functions split along natural lines:

| Concern | Old backing store | New backing store |
|---|---|---|
| `SetPending`, `GetPending`, `DeletePending` | Redis HASH + TTL | Postgres `pending_deploys` |
| `PushHistory`, `GetHistory`, `FindHistoryBySHA` | Redis LIST | Postgres `history` |
| `AcquireLock`, `ReleaseLock`, `TryLock` | Redis SET NX EX | **Redis, unchanged** |
| `SetThreadTS`, `GetThreadTS` | Redis STRING | **Redis, unchanged** |
| Approvers cache | Redis SET | **Redis, unchanged** |
| Identity cache | Redis HASH | **Redis, unchanged** |
| Dedupe markers (ArgoCD, ECR) | Redis STRING NX | **Redis, unchanged** |
| Streams | Redis Streams | **Redis, unchanged** |

The split is clean because each persistence concern is already isolated
to its own file in `internal/store/`. No cross-calls today, no dual-write
logic to retrofit.

**New files:**

- `internal/store/postgres/postgres.go` — `pgxpool.Pool` setup, auth
  handling (password or IAM), connection lifecycle, health check.
- `internal/store/postgres/history.go` — `PushHistory`, `GetHistory`,
  `FindHistoryBySHA`, plus the `app + environment + completed_at <
  threshold` query that phase-4 rollback uses.
- `internal/store/postgres/pending.go` — `SetPending`, `GetPending`,
  `DeletePending`, `ExpireStalePending` (replacement for the Redis
  TTL-based expiration).
- `internal/store/postgres/retention.go` — `Retainer` type, ticker
  loop, pre-flight + batched-delete logic.
- `internal/store/postgres/migrations/*.sql` — goose migrations.

**Removed files:**

- `internal/store/history.go` (Redis version) — content moves to
  `postgres/history.go`.
- `internal/store/pending.go` (Redis version) — content moves to
  `postgres/pending.go`.

**Unchanged:**

- `internal/store/lock.go`, `thread.go`, `approvers.go` (if present), and
  any other file whose only concern is short-TTL Redis state.

Callers of `store.*` methods don't change — the `Store` struct still has
the same method set, and the dispatch to Redis vs. Postgres is an
internal detail. Tests can keep using `miniredis` for the Redis-backed
portions, and gain a Postgres testcontainer for the Postgres-backed
ones — see [Testing](#testing).

## Auth (Postgres)

Match the existing Redis pattern exactly: password by default, IAM auth
as an optional alternative.

**New config under `config.json`:**

```json
{
  "postgres": {
    "host": "deploy-bot-db.example.com",
    "port": 5432,
    "database": "deploy_bot",
    "user": "deploy_bot",
    "sslmode": "require"
  }
}
```

**New secrets under the receiver and bot secret files:**

```json
{
  "postgres_password": "...",
  "postgres_iam_auth": false
}
```

When `postgres_iam_auth` is `true`, the password field is ignored and
the DSN is built against AWS RDS IAM-auth tokens, refreshed on a
schedule by a wrapper similar to `internal/store.redisIAMAuth` (exact
interface TBD during implementation but modelled after the Redis
counterpart — any operator who has set up IAM auth for Redis will find
the Postgres version identical in shape).

Same approach for scoping: bot and receiver get separate database roles
with only the privileges they need. Receiver typically writes to
`pending_deploys` (modal submit), bot reads/updates both tables and runs
retention. Principle of least privilege at the DB level as well as the
secrets level.

## Migrations

**Tool:** [`goose`](https://github.com/pressly/goose). Schema is small
(two tables to start) and unlikely to grow quickly. Goose is the
ergonomic middle ground — file-based migrations with up/down directions,
tracks state in a `goose_db_version` table, no runtime DSL to learn.

**Invocation:** at bot startup, run `goose.Up()` automatically against
the configured Postgres. Migrations are small, fast, and idempotent; no
reason to make operators run a separate step. A feature-flag
`postgres.auto_migrate: false` lets operators disable this and run
migrations out-of-band for stricter change control, but the default is
auto-migrate for operational simplicity.

**File layout:**

```
internal/store/postgres/migrations/
  00001_create_history.sql
  00002_create_pending_deploys.sql
```

**One-shot v1 → v2 data migration:**

A standalone command at `cmd/migrate-redis-to-postgres/main.go`:

1. Reads bot config + secrets from `CONFIG_PATH` / `SECRETS_PATH` /
   `AWS_SECRET_NAME` — same lookup path the bot uses.
2. Connects to both Redis (source) and Postgres (destination).
3. `LRANGE history 0 -1`, decodes each entry, inserts into
   `history` table with `inserted_at = now()` and `completed_at` from
   the source record.
4. `SCAN pending:*`, decodes each hash, inserts into `pending_deploys`
   with `state = 'pending'` and `expires_at` computed from the Redis
   TTL on the source key.
5. Idempotent: re-running is safe (uses `ON CONFLICT DO NOTHING` on
   history `id`-surrogate, and `ON CONFLICT (pr_number) DO NOTHING` on
   pending).
6. Emits a summary of rows inserted + any decode errors.

Published as part of the 2.0 release. Operators run it once during the
upgrade, downtime window: zero (the bot can be running on 1.x during the
migration; just cut over to 2.0 after it completes).

**Release-notes outline for 2.0:**

- Breaking: Postgres is required. Set up a database (a `postgres:15-alpine`
  container is sufficient), populate `postgres` in config and
  `postgres_password` in secrets.
- Migration: run `./bin/migrate-redis-to-postgres` once before starting
  the new bot. Can be run against a live 1.x bot; the migration is
  read-only on the Redis side.
- Removed: Redis no longer needs to be durable. Operators running Redis
  with AOF/RDB for deploy-bot's sake can turn both off.
- New: `/deploy history` supports larger limits (up to 500 instead of
  100) and filters by environment. Retention defaults to 2 years. The
  "last 100 deploys" cap is gone.

## Testing

Match the Redis setup pattern exactly, as you requested. Two pieces:

**Unit tests** — `miniredis` stays where it is for the Redis-backed
code. Postgres-backed code uses `testcontainers-go/postgres` to spin up
an ephemeral container per test package, identical to how `miniredis` is
used today. Tests in `internal/store/postgres/*_test.go` get a fresh
schema per test function via a `TRUNCATE ... CASCADE` reset.

`testcontainers-go` adds a Docker/Podman dependency to `go test`. That's
acceptable given the rest of the test infrastructure — the integration
tests already depend on a real Redis. For `make test` (unit tests only),
we'll gate the Postgres tests behind a build tag or skip-if-unavailable
check so `make test` on a machine without a container runtime still
runs cleanly, same as miniredis today.

**Integration tests** — `tests/integration/` grows a Postgres container
alongside the existing Redis container. `.env.integration` gains a
`POSTGRES_DSN` field. Everything else stays the same.

**CI** — `.github/workflows/pr.yml` already has a test job. It gains a
`services.postgres` block matching the existing `services.redis`. The
service container starts with a known password and the test harness
reads it from env. No secret management needed for CI; it's a throwaway
database.

## Open questions

Things I'd want to close before cutting code, not blocking for the doc
review:

1. **Database per environment or shared?** Dev/staging/prod all get their
   own Postgres, or share one with a `environment` column and
   row-level filtering? Leaning "one per environment, mirrors how Redis
   is deployed today." Decide during implementation.

2. **`pr_number` as primary key.** It's unique within a given GitHub repo,
   but if deploy-bot ever manages apps across multiple gitops repos in
   the same bot instance, PR numbers can collide. Today's code has the
   same assumption in its `pending:<pr>` Redis key, so this is not a
   regression — but adding a `(pr_number, github_org, github_repo)`
   composite primary key from day one costs nothing and future-proofs
   against multi-repo tenancy. Mild lean toward composite.

3. **JSONB sidecar for unknown fields.** Should `history` carry a `metadata
   JSONB` column for future-additive fields we haven't thought of yet?
   Argument for: cheap, saves one migration per new field. Argument
   against: it's an attractive nuisance for schemaless drift. Lean
   against — we use `ALTER TABLE ADD COLUMN` when we need a new field.

4. **Long-running `SELECT FOR UPDATE SKIP LOCKED` in the sweeper.**
   The sweeper currently runs every 5 minutes. `FOR UPDATE SKIP LOCKED`
   is right for multi-worker safety, but holds row locks for the
   duration of the handler. Handler is fast (a few Slack posts + a
   GitHub close PR call), so holding locks for a few seconds is fine.
   Worth confirming the handler is genuinely fast and not accidentally
   synchronous on anything slow. Quick audit at implementation time.

5. **Multi-version table schemas during the rolling upgrade.** None of
   the existing 2.0 work requires it because we're doing a hard cut, but
   the next schema change (2.1 or whatever) will. Worth defining the
   policy now — additive changes only, `ADD COLUMN NULLABLE` or `ADD
   COLUMN ... DEFAULT ...`, no destructive changes in the same release
   as code that stops writing the column. Standard stuff, just document
   it.

6. **`auto_migrate` on by default or off?** I'm defaulting to on because
   it reduces operator friction, but stricter shops might want
   out-of-band migrations. Gated behind a config flag. Default decision
   stands unless operator feedback says otherwise.

## Cost / effort estimate

Back-of-envelope sizing:

- Schema + goose migration wiring: ~150 LoC.
- `internal/store/postgres/` package (connection, history, pending,
  retention): ~600 LoC.
- Test updates (unit, integration, CI): ~300 LoC.
- `cmd/migrate-redis-to-postgres`: ~200 LoC.
- Doc updates (`production-setup.md`, `configuration.md`, new
  `postgres-setup.md`, release notes for 2.0): ~400 lines of prose.
- Redis cleanup (remove `internal/store/history.go`,
  `internal/store/pending.go`, update callers): ~-300 LoC net.

Total: **somewhere around 1400-1600 LoC net new**, most of it in the
postgres package and its tests. Small doc prose churn. Probably 3-4
reviewable PRs:

1. `feat(store): add postgres package behind feature flag` — schema,
   pool, one method each for history + pending, no cutover yet, tests
   for the new code only. Bot still reads/writes Redis.
2. `feat(store): cut history over to postgres, remove redis list` —
   removes `internal/store/history.go`, updates callers, gates behind
   `postgres.enabled` until the release.
3. `feat(store): cut pending over to postgres, update sweeper` — same
   shape, for pending.
4. `feat: require postgres (deploy-bot 2.0)` — flips defaults, removes
   the feature flag, includes `cmd/migrate-redis-to-postgres`, updates
   docs, bumps the module version.

Each is independently reviewable and the middle two can land on main
without breaking 1.x behaviour (the feature flag defaults off). Only the
last PR is the breaking change.

## What this doc is not

- A final spec. The schemas, column types, and code layouts are all
  reasonable first-draft choices. Reviewers should push back on any of
  them.
- An implementation plan with a timeline. I've been told not to predict
  schedules; this is about shape, not sequencing.
- A commitment to all the open questions resolving as written. I've
  flagged the ones I'm unsure about so they get closed during code
  review, not assumed away.

## Decisions locked in by this doc

- Postgres is **required** on 2.0, not optional.
- `pgx` native (not `database/sql` + `lib/pq`).
- `goose` for migrations.
- IAM auth **optional**, password default — mirrors the existing Redis
  pattern.
- Test harness matches Redis (service container in CI, testcontainers
  for unit tests where applicable, `.env.integration` for integ).
- Retention is a **background ticker**, not a slash command.
- Retention default **2 years**, hard floor **390 days** enforced at
  config load.
- **Hard-delete**, not soft-delete. Safety via config-load floor +
  pre-flight log + Postgres backups.
- **Separate tables** for `history` and `pending_deploys`, not a single
  table with a `state` column.
- **Hard cut** on a major version bump, not dual-write. One `cmd/migrate-
  redis-to-postgres` command, run once during the 1.x → 2.0 upgrade.
