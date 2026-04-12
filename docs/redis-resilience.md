# Redis resilience

deploy-bot requires Redis for event streaming, per-app locks, caches, and
short-TTL coordination. On startup, both the receiver and worker attempt to
connect with retries every 5 seconds for up to 60 seconds. During this window
the pod reports unhealthy (`/healthz` returns 503) and not ready (`/readyz`
returns 503). If Redis is still unreachable after 60 seconds, the process
exits. There is no degraded mode; Redis is not optional.

> **3.0+ note:** Durable state (deploy history, in-flight pending deploys) is
> now in Postgres, not Redis. Redis no longer needs AOF or RDB persistence —
> it can run as a pure ephemeral cache + queue. A Redis flush or restart loses
> streams and locks but not history or pending deploys. See
> [postgres-setup.md](postgres-setup.md) for the durable-store setup.

## What Redis holds (3.0+)

| Data | Key pattern | TTL |
|---|---|---|
| Per-app deploy locks | `lock:<env>/<app>` | `lock_ttl` (default 5m) |
| System locks (sweeper, reconcile, retention) | `syslock:<name>` | Varies |
| User event stream | `user:events` | MAXLEN ~10,000 entries |
| ECR event stream | `ecr:events` | MAXLEN ~10,000 entries |
| ArgoCD event stream | `argocd:events` | MAXLEN ~10,000 entries |
| Consumer groups | (stream metadata) | Permanent until deleted |
| Deploy thread parents | `thread:<env>` | 10 minutes |
| ArgoCD dedupe markers | `argocd:seen:<app>:<sha>:<trigger>` | 24 hours |
| Identity / approver caches | various | Refreshed periodically |

Everything above is either reconstructible (caches refresh on their own,
consumer groups are re-created on startup) or short-lived (locks, thread
parents, dedupe markers all have TTLs). The only data that matters for
correctness is the stream content — events in-flight between the receiver and
worker. The receiver's in-memory buffer and Slack's at-least-once retries
cover the gap when streams are lost.

## Temporary unavailability

If Redis becomes unreachable after startup (e.g. a primary node failure during
ElastiCache failover, typically 1–2 minutes):

**Receiver**
- New Slack events cannot be enqueued (XADD fails).
- Failed events are placed in an in-memory buffer (default 500 events,
  configurable via `slack.buffer_size`).
- The buffer drains automatically with exponential backoff (1s → 30s) once
  Redis recovers.
- Slack is never ACKed from the buffer — Slack retries unACKed events in
  parallel, providing a second delivery path if the receiver restarts while
  the buffer is non-empty.
- If the buffer fills (extended outage or very high event rate), incoming
  events are dropped. Slack will retry these since they were never ACKed.

**Worker**
- The event loop continues running but `XReadGroup` calls fail.
- Errors are logged and the loop retries on each iteration.
- If the Redis consumer group is missing on recovery (see flush scenario
  below), the worker re-initialises it automatically.
- In-flight events that were claimed but not yet ACKed before the outage are
  recovered via `XAUTOCLAIM` once Redis is back (claim idle threshold: 60s).
- Pending-deploy and history operations (Postgres-backed) are **unaffected**
  by Redis outages. `/deploy history`, rollback target resolution, and ArgoCD
  notification correlation continue to work.

**Sweeper**
- The sweeper queries Postgres for expired deploys, so its data path is
  unaffected by Redis outages. However, it acquires a Redis sys-lock to
  coordinate multi-replica execution — if Redis is down, the lock acquisition
  fails and the sweep is skipped for that tick. Stale deployments are not
  expired until the next tick where Redis is available for the lock. No data
  is lost; the sweep catches up on recovery.

## FLUSHALL / data loss

If Redis is flushed (`FLUSHALL` or equivalent), all Redis-held state is lost.
**Deploy history and pending deploys are unaffected** — they live in Postgres.

What is lost and how it recovers:

### 1. Consumer group re-initialisation

The worker detects the missing consumer group (`NOGROUP` error on
`XReadGroup`) and calls `Init` to recreate it. The stream is empty after a
flush so no events are replayed — any events that arrived during the outage
window are covered by the receiver buffer and Slack's retries.

### 2. Per-app deploy locks

All `lock:<env>/<app>` keys are gone. This means a second deploy for the same
app could be submitted while a first is still in-flight. The Postgres
`pending_deploys` table is the authoritative record of in-flight deploys, and
the bot's `GetByEnvApp` check (which queries Postgres) will still surface the
conflict on modal submission. The lock is a fast-path optimisation, not the
sole guard.

### 3. Thread parent timestamps

`thread:<env>` keys are gone, so the next deploy in each environment will post
a new top-level message instead of threading under an existing one. Cosmetic
impact only; no data loss.

### 4. Dedupe markers

ArgoCD and ECR dedupe markers (`argocd:seen:*`, `ecr:seen:*`) are gone. The
next notification or ECR event for a recently-processed SHA/tag may be
re-delivered and processed a second time. For ArgoCD, this means a duplicate
Slack post (annoying but not harmful). For ECR, a duplicate PR creation
attempt that the GitHub API rejects as a conflict (the branch already exists).

### 5. GitHub reconciliation

`ReconcileFromGitHub` still runs on startup and queries GitHub for open PRs
carrying the `deploy-bot/pending` label. Since pending deploys are in
Postgres, this path is now a sanity check rather than a primary recovery
mechanism — it catches PRs where Postgres and GitHub disagree (e.g. a PR was
closed outside the bot).

### 6. History reconstruction (rarely needed)

`ReconstructHistory` detects an empty Postgres history table and
asynchronously rebuilds from GitHub commit history. Since a Redis flush does
NOT empty the Postgres history table, this path does not trigger on a
Redis-only event. It only runs after a Postgres data loss (which would
typically be restored from a database backup instead).

### Optional periodic reconciliation

Reconciliation also runs on a configurable interval if
`deployment.reconcile_interval` is set (e.g. `1h`). This is disabled by
default. Only one worker pod runs reconciliation per interval, coordinated
via a Redis lock.

## Recommended ElastiCache configuration

Since Redis no longer holds durable state, the operational posture is simpler
than in 2.x:

- **Multi-AZ with automatic failover** — primary failure triggers automatic
  promotion of a replica, typically within 1–2 minutes. This is for
  **availability**, not durability.
- **AOF / RDB persistence is optional.** Redis holds only streams, locks, and
  caches — all of which are either short-lived or reconstructible. Enabling
  AOF avoids re-delivery of in-flight stream events after a node restart, but
  the dedupe layer and Slack's retries already handle that case. If your
  ops team prefers the belt-and-suspenders approach, `appendfsync everysec`
  is fine; if you'd rather simplify, turning persistence off is equally
  valid.
- **Do not run FLUSHALL in production** without coordinating with the team.
  The blast radius is smaller than in 2.x (no history or pending deploy
  loss), but in-flight events will be re-delivered and deploy locks will
  momentarily drop.
