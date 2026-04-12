# Postgres Setup

deploy-bot 2.0 requires Postgres for durable storage of deploy history
and in-flight pending deploys. Redis remains required for streams,
caches, locks, and short-TTL coordination.

## Quick start (container)

```bash
# Start a local Postgres for development / testing:
podman run -d --name deploy-bot-pg \
  -e POSTGRES_DB=deploy_bot \
  -e POSTGRES_USER=deploy_bot \
  -e POSTGRES_PASSWORD=changeme \
  -p 5432:5432 \
  postgres:15-alpine
```

Then add to your `config.json`:

```json
{
  "postgres": {
    "host": "localhost",
    "database": "deploy_bot",
    "user": "deploy_bot"
  }
}
```

And to your secrets file:

```json
{
  "postgres_password": "changeme"
}
```

## Config reference

| Field | Required | Default | Description |
|---|---|---|---|
| `postgres.host` | yes | — | Hostname or IP of the Postgres server |
| `postgres.port` | no | `5432` | Port |
| `postgres.database` | yes | — | Database name |
| `postgres.user` | yes | — | Database user |
| `postgres.sslmode` | no | `require` | One of `disable`, `allow`, `prefer`, `require`, `verify-ca`, `verify-full` |
| `postgres.auto_migrate` | no | `false` | Run goose migrations at bot startup (see [Migrations](#migrations)) |
| `postgres.retention_history` | no | `17520h` (2y) | Max age of history rows before purge. Must be ≥ `9360h` (390 days) |

### Secrets

| Field | Required | Default | Description |
|---|---|---|---|
| `postgres_password` | yes (unless IAM) | — | Database password |
| `postgres_iam_auth` | no | `false` | Use AWS RDS IAM auth tokens instead of a static password |
| `postgres_rds_region` | when IAM | — | AWS region for the SigV4 signer |

## Migrations

Schema migrations are embedded in the bot binary and managed by
[goose](https://github.com/pressly/goose). They run **only when
`postgres.auto_migrate` is `true`** — the default is `false`.

### Upgrade workflow

1. Set `"auto_migrate": true` in your config.
2. Restart the bot. It acquires a Postgres advisory lock so only one
   replica runs migrations during a rolling restart. You'll see:
   ```
   postgres: migrations applied  duration=150ms
   ```
3. Set `"auto_migrate": false` and restart again (or leave it for the
   next deploy cycle to pick up via config hot-reload — note that the
   `postgres` section itself is **not** hot-reloadable, but
   `auto_migrate` is read at startup only anyway).

The receiver **never** runs migrations, even if `auto_migrate` is true.

### Advisory lock

Migrations are serialized via `pg_advisory_lock`. If multiple bot
replicas restart simultaneously, exactly one acquires the lock and
runs goose; the others block until it finishes, then proceed with a
no-op migration check. The lock is session-scoped — if the winning
replica crashes mid-migration, the server releases the lock when the
connection drops.

## 1.x → 2.0 data migration

A standalone tool copies existing Redis data to Postgres:

```bash
CONFIG_PATH=/etc/deploy-bot/config.json \
SECRETS_PATH=/etc/deploy-bot/secrets.json \
./bin/migrate-redis-to-postgres
```

The tool:

- Reads `history` (Redis LIST) → inserts into `history` table
- Reads `pending:*` (Redis HASHes) → inserts into `pending_deploys` table
- Populates `github_org` / `github_repo` from `config.github.org` /
  `config.github.repo` (1.x is single-repo-by-definition)
- Is **idempotent**: uses `ON CONFLICT DO NOTHING`, so re-running after
  a partial failure is safe
- Can run against a **live 1.x bot** — it only reads from Redis

### Upgrade sequence

1. Deploy Postgres (container or RDS).
2. Add `postgres` block to config + secrets.
3. Run `migrate-redis-to-postgres` once.
4. Set `auto_migrate: true`, deploy 2.0 bot + receiver.
5. Verify `/deploy history` shows migrated data.
6. Set `auto_migrate: false` on next config push.
7. (Optional) Turn off Redis AOF/RDB — Redis no longer needs to be
   durable.

## Retention

The bot runs a background retention ticker (once per 24 hours, gated
by a Redis sys-lock so only one replica runs it) that purges history
rows older than `postgres.retention_history`.

- **Default:** 2 years (`17520h`)
- **Minimum:** 390 days (`9360h`) — enforced at startup. The floor
  exists because audits don't happen immediately at period end; 390
  days covers the audit lag without risking early deletion of
  compliance-relevant data.
- **No slash command.** Retention is an ops concern, not a developer
  action. If you need to run it manually, `deploy-bot-config` will
  gain a `retention` subcommand in a future release.

`pending_deploys` has no retention policy — rows are deleted by the
bot on state transition (the matching terminal record goes into
`history`).

## AWS RDS IAM auth

When `postgres_iam_auth` is `true`, the bot generates short-lived
(~15 min) auth tokens via the AWS RDS IAM signing flow instead of
using a static password. This mirrors the existing Redis ElastiCache
IAM auth pattern.

Requirements:

- `postgres_rds_region` must be set in secrets
- The bot's IAM identity must have `rds-db:connect` permission for
  the target database user
- `sslmode` should be `require` or stricter (IAM tokens are only
  valid over TLS)
- The RDS instance must have IAM authentication enabled

Tokens are refreshed on every new physical connection via pgx's
`BeforeConnect` hook — no background token-refresh goroutine is
needed.
