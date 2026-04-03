# Redis resilience

deploy-bot requires Redis to function. On startup, both the receiver and worker
attempt to connect to Redis with retries every 5 seconds for up to 60 seconds.
During this window the pod reports both unhealthy (`/healthz` returns 503) and
not ready (`/readyz` returns 503). If Redis is still unreachable after 60
seconds, the process exits. Kubernetes liveness probes will restart the pod if
the failure threshold is reached before the 60-second timeout, which is the
expected behaviour — the restart backoff gives Redis time to come up. There is
no degraded mode; Redis is not optional.

## What Redis holds

| Data | Key pattern | TTL |
|---|---|---|
| Pending deployments | `pending:<pr_number>` | `stale_duration` (default 2h) |
| Per-app deploy locks | `lock:<env>/<app>` | `lock_ttl` (default 5m) |
| System locks (sweeper, reconcile) | `syslock:<name>` | Varies |
| Event stream | `slack:events` | MAXLEN ~10,000 entries |
| Consumer group | (stream metadata) | Permanent until deleted |
| Deployment history | `history` | Permanent (capped at 100 entries) |

## Temporary unavailability

If Redis becomes unreachable after startup (e.g. a primary node failure during
ElastiCache failover, typically 1–2 minutes):

**Receiver**
- New Slack events cannot be enqueued (XADD fails).
- Failed events are placed in an in-memory buffer (default 500 events, configurable via `slack.buffer_size`).
- The buffer drains automatically with exponential backoff (1s → 30s) once Redis recovers.
- Slack is never ACKed from the buffer — Slack retries unACKed events in
  parallel, providing a second delivery path if the receiver restarts while the
  buffer is non-empty.
- If the buffer fills (extended outage or very high event rate), incoming events
  are dropped. Slack will retry these since they were never ACKed.

**Worker**
- The event loop continues running but `XReadGroup` calls fail.
- Errors are logged and the loop retries on each iteration.
- If the Redis consumer group is missing on recovery (see flush scenario below),
  the worker re-initialises it automatically.
- In-flight events that were claimed but not yet ACKed before the outage are
  recovered via `XAUTOCLAIM` once Redis is back (claim idle threshold: 60s).

**Sweeper**
- The expiry sweep is skipped for any tick where Redis is unavailable.
- Stale deployments are not expired until the next successful sweep.
- No data is lost; the sweep catches up on recovery.

## FLUSHALL / data loss

If Redis is flushed (`FLUSHALL` or equivalent), all state is lost. The
following recovery sequence runs automatically on the next worker startup:

### 1. Consumer group re-initialisation

The worker detects the missing consumer group (`NOGROUP` error on `XReadGroup`)
and calls `Init` to recreate it. The stream is empty after a flush so no events
are replayed — any events that arrived during the outage window are covered by
the receiver buffer and Slack's retries.

### 2. Stuck-merge recovery

`RecoverStuck` scans Redis for deployments in `merging` state and attempts to
merge their PRs. After a flush there are no Redis entries, so this pass is a
no-op. Any PR that was genuinely mid-merge at flush time will have been merged
on GitHub already (or will require manual review).

### 3. GitHub reconciliation

`ReconcileFromGitHub` queries GitHub for open PRs carrying the `deploy-bot/pending`
label (configurable via `deployment.label`). These are PRs the bot created that
have not yet been approved, rejected, cancelled, or expired.

For each such PR that is absent from Redis:

- The PR is **closed** on GitHub and the pending label is removed.
- The per-app lock is released.
- The requester receives a DM with the exact command to re-request:
  `/deploy <app>` selecting tag `<tag>`.

If multiple PRs for the same app are found, each requester's DM includes a list
of the concurrent requests so they can coordinate before re-requesting.

> **Note:** The `deploy-bot/pending` label is only present on open PRs. Closed
> PRs (approved, rejected, cancelled, expired) retain only the `deploy-bot`
> audit label and are never surfaced by reconciliation.

### 4. History list reconstruction

The deployment history (`/deploy history`) is empty after a flush. On startup,
the worker detects the empty history list and asynchronously reconstructs it
from GitHub commit history (`ReconstructHistory`). It scans deploy commits
for each configured app's kustomize path, extracts app/tag/environment from
the commit message format `deploy(<env>/<app>): update image tag to <tag>`,
and populates history entries. Reconstructed entries are missing requester IDs
and PR links since those are not derivable from commit messages alone.

### Optional periodic reconciliation

Reconciliation also runs on a configurable interval if `deployment.reconcile_interval`
is set (e.g. `1h`). This is disabled by default. Only one worker pod runs
reconciliation per interval, coordinated via a Redis lock.

## Recommended ElastiCache configuration

To minimise the impact of node failures and make data loss scenarios unlikely:

- **Multi-AZ with automatic failover** — primary failure triggers automatic
  promotion of a replica, typically within 1–2 minutes.
- **AOF persistence enabled** (`appendonly yes`) — data survives a node restart.
  Use `appendfsync everysec` for a balance of durability and performance.
- **Do not run FLUSHALL in production.** If you must, coordinate with the team
  first — requesters with pending deployments will need to re-request.
