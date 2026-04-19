# Read-only HTTP API (`bin/api`)

`cmd/api` exposes a read-only HTTP surface over deploy-bot's Postgres
state. It is intended for a separate UI running in the same Kubernetes
cluster. The API does not connect to Redis, Slack, GitHub, or ECR, and
does not run migrations — it reads `history` and `pending_deploys` and
nothing else.

Build with `make api` (or `make build`, which now also produces
`bin/api`).

## Auth

The API does not implement a user-facing login flow. The UI performs
OIDC (authorization code flow) against the IdP, obtains an `id_token`,
and forwards it to the API on each request:

```
Authorization: Bearer <id_token>
```

The middleware verifies the token against the issuer's JWKS and
expected audience on every request. No session is established.

Run-time configuration:

| Env var | Required | Description |
|---|---|---|
| `OIDC_ISSUER_URL` | yes | IdP issuer URL (the `.well-known/openid-configuration` discovery base). |
| `OIDC_AUDIENCE` | yes | Expected `aud` claim — typically the UI's OIDC client_id. |

All authenticated users currently see all data. Add a group check
around `Routes()` if you need per-group scoping.

## Endpoints

All response bodies are JSON.

| Method | Path | Description |
|---|---|---|
| GET | `/v1/apps` | List configured apps (operator + repo-discovered) |
| GET | `/v1/apps/{appEnv}/history?limit=N` | History for `myapp-dev` (composite name). `limit` defaults to 50, max 500 |
| GET | `/v1/apps/{appEnv}/pending` | In-flight pending deploys for the given app |
| GET | `/v1/deploys/{org}/{repo}/{pr}` | Single pending deploy by GitHub PR coordinates |
| GET | `/v1/history?sha=<gitops_sha>` | History entry for a merge commit SHA |

Admin endpoints are served on a separate port (`METRICS_ADDR`, default
`:9091`) and do not require OIDC:

- `GET /healthz` — liveness
- `GET /readyz` — readiness
- `GET /metrics` — Prometheus exposition

The primary API listens on `API_ADDR` (default `:8080`).

### Response shapes

Responses mirror the types in `internal/store`
([`HistoryEntry`](../internal/store/types.go),
[`PendingDeploy`](../internal/store/types.go)). The `/v1` path prefix
is the stability contract — schema changes that rename fields in the
store must update the handler projection to keep `/v1` stable.

Errors are returned as `{"error": "<message>"}` with an appropriate
status code.

## Environment variables

| Env var | Required | Default | Description |
|---|---|---|---|
| `CONFIG_PATH` | no | `/etc/deploy-bot/config.json` | Primary config file |
| `SECRETS_PATH` | one of | — | File-based secrets (alternative to `AWS_SECRET_NAME`) |
| `AWS_SECRET_NAME` | one of | — | Name of an AWS Secrets Manager secret containing the secrets blob |
| `API_ADDR` | no | `:8080` | Listen address for the authenticated API |
| `METRICS_ADDR` | no | `:9091` | Listen address for health + metrics |
| `OIDC_ISSUER_URL` | yes | — | OIDC issuer discovery URL |
| `OIDC_AUDIENCE` | yes | — | Expected `aud` claim |

Only the Postgres fields of the secrets blob are consulted. You can —
and should — point the API at a secret that contains read-only DB
credentials (see below), not the bot's read/write secret.

## Read-only Postgres role

[`docs/sql/api_reader.sql`](sql/api_reader.sql) creates an `api_reader`
role with `SELECT` on `history` and `pending_deploys` (and default
privileges for any future tables). It supports both password and AWS
RDS IAM auth; pick one and uncomment the relevant line before applying.

Apply once per environment:

```bash
psql -h <host> -U <owner> -d deploy_bot -f docs/sql/api_reader.sql
```

## Local development

```bash
# Start deploy_bot Postgres if you don't have one running:
podman run -d --name deploy-bot-pg \
  -e POSTGRES_DB=deploy_bot \
  -e POSTGRES_USER=deploy_bot \
  -e POSTGRES_PASSWORD=changeme \
  -p 5432:5432 postgres:15-alpine

# Run the bot once so migrations create the tables:
CONFIG_PATH=./config.json SECRETS_PATH=./secrets.json bin/bot

# Apply the read-only role:
psql -h localhost -U deploy_bot -d deploy_bot -f docs/sql/api_reader.sql

# Run the API pointed at a secrets file that uses api_reader creds:
CONFIG_PATH=./config.json \
  SECRETS_PATH=./secrets.api.json \
  OIDC_ISSUER_URL=https://accounts.google.com \
  OIDC_AUDIENCE=<ui-client-id> \
  bin/api
```

Hit an endpoint with a valid ID token:

```bash
curl -H "Authorization: Bearer $ID_TOKEN" http://localhost:8080/v1/apps
```
