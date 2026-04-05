# GitHub App Authentication

By default, deploy-bot authenticates to GitHub using fine-grained personal access tokens (PATs). This works well for most deployments, but organizations running at scale may benefit from using a GitHub App instead.

## Why consider a GitHub App

### Rate limits

GitHub rate limits are per-token:

| Auth method | Primary rate limit | Secondary rate limits |
|---|---|---|
| Fine-grained PAT | 5,000 requests/hour | Shared across all tokens owned by the same user |
| GitHub App installation | 5,000+ requests/hour (scales with org size, up to 12,500) | Independent per installation |

deploy-bot already uses separate tokens for the worker (`github_token`) and scanner (`github_scanner_token`), which helps spread primary rate limit usage. However, because both PATs belong to the same user, they share secondary rate limits (GitHub's abuse detection limits on concurrent requests to the same endpoint). A GitHub App avoids this -- its installation tokens have fully independent rate limits.

The repo scanner is the heaviest consumer: paginating org repos, fetching config files, and setting commit statuses. At ~200+ repos, scanner activity alone can push close to the 5,000/hour PAT limit, especially during the initial scan after a restart.

### Operational benefits

- **No personal account dependency.** PATs are tied to a user. If that person leaves the org or rotates their token, the bot breaks. A GitHub App is owned by the organization.
- **Granular permissions.** GitHub App permissions are declared at installation time and visible in the org settings. No guessing which scopes a PAT was created with.
- **Automatic token rotation.** Installation tokens expire after 1 hour and are re-generated automatically. No manual rotation, no long-lived credentials.
- **Audit trail.** GitHub App actions appear as the app in audit logs, not as a personal user.

## When PATs are fine

- Small to mid-size orgs (under ~100 repos scanned)
- Repo discovery disabled (scanner is the main rate limit consumer)
- Single-team setups where the bot operator controls the PAT lifecycle

## GitHub App setup

### 1. Create the app

Go to your org settings: **Settings > Developer settings > GitHub Apps > New GitHub App**.

| Setting | Value |
|---|---|
| GitHub App name | `deploy-bot` (or your preferred name) |
| Homepage URL | Your docs site or repo URL |
| Webhook | **Inactive** (deploy-bot uses Socket Mode and SQS, not webhooks) |

### 2. Set permissions

**Repository permissions:**

| Permission | Level | Used by |
|---|---|---|
| Contents | Read & write | Worker (PR branches, commits), Scanner (config file reads) |
| Pull requests | Read & write | Worker (create, merge, close PRs) |
| Issues | Read & write | Worker (labels) |
| Commit statuses | Read & write | Scanner (conflict status checks) |

**Organization permissions:**

| Permission | Level | Used by |
|---|---|---|
| Members | Read | Worker and Receiver (team membership checks for deployer/approver gating) |

### 3. Generate a private key

After creating the app, generate a private key (PEM file) from the app settings page. Store it securely -- this is used to mint short-lived installation tokens.

### 4. Install the app

Install the app on your organization. You can scope it to:

- **All repositories** -- required if using repo discovery without `repo_prefix`
- **Selected repositories** -- choose the gitops repo and any repos the scanner should discover

### 5. Note the IDs

You'll need:
- **App ID** -- shown on the app settings page
- **Installation ID** -- visible in the URL after installing (`https://github.com/settings/installations/<ID>`)
- **Private key** -- the PEM file from step 3

### 6. Token generation

GitHub App authentication is a two-step process:

1. Sign a JWT with the app's private key (valid for 10 minutes)
2. Exchange the JWT for an installation token (valid for 1 hour)

deploy-bot does not yet have built-in GitHub App token generation. You have two options for providing installation tokens:

**Option A: External token broker (recommended)**

Run a sidecar or CronJob that generates installation tokens and writes them to the secret. Tools like [github-app-token](https://github.com/tibdex/github-app-installation-token) or a simple script can handle this:

```bash
# Example: generate token and update Secrets Manager
TOKEN=$(generate-installation-token --app-id 12345 --key /path/to/key.pem --installation-id 67890)

aws secretsmanager put-secret-value \
  --secret-id deploy-bot/bot-secrets \
  --secret-string "$(jq -n \
    --arg token "$TOKEN" \
    --arg slack "xoxb-..." \
    --arg redis "redis:6379" \
    '{github_token: $token, slack_bot_token: $slack, redis_addr: $redis}')"
```

Run this on a schedule shorter than the 1-hour token expiry (e.g. every 30 minutes). The bot picks up the new token on the next GitHub API call.

**Option B: GitHub Actions workflow token**

If the bot only needs GitHub access during CI-triggered flows, you can use the `actions/create-github-app-token` action to generate tokens in workflows. This doesn't help with the always-running bot process, but can supplement PATs for specific operations.

## Secret configuration

The secret fields are the same regardless of auth method. Replace the PAT values with installation tokens:

```json
{
  "github_token": "<installation-token>",
  "github_scanner_token": "<installation-token>"
}
```

Both fields can use the same installation token if the app is installed with all required permissions. Using separate tokens (from separate app installations or with different repository scoping) provides further rate limit isolation.

## Monitoring rate limits

Watch for these log messages that indicate rate limit pressure:

- **Scanner:** `rate limit remaining below floor, pausing scan` -- the scanner's `rate_limit_floor` (default 500) was hit
- **Worker:** `GitHub secondary rate limit` -- concurrent request limit reached, retrying with backoff

If you see these frequently with PATs, switching to a GitHub App is the recommended next step.
