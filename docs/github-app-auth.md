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

## Secret configuration

Store the App ID, Installation ID, and private key in your secret. These replace `github_token`:

```json
{
  "slack_bot_token": "xoxb-...",
  "slack_app_token": "xapp-...",
  "github_app_id": 12345,
  "github_app_installation_id": 67890,
  "github_app_private_key": "-----BEGIN RSA PRIVATE KEY-----\nMIIE...\n-----END RSA PRIVATE KEY-----",
  "redis_addr": "redis:6379"
}
```

Installation tokens are minted on demand, cached for up to 1 hour, and refreshed before expiry.

Both `github_token` (PAT) and the App fields can coexist during migration. When App credentials are present, they take precedence. The `github_scanner_token` PAT can still be set independently if you want the scanner to use a separate identity.

## Per-component token scoping

Each component requests only the permissions it needs when minting its installation token, regardless of the broader permissions the App was installed with.

| Component | Token permissions | Purpose |
|---|---|---|
| Worker (bot + sweeper) | contents:write, pull_requests:write, issues:write, members:read | PR creation, merge, close, labels, team membership checks |
| Validator | members:read | Slack-to-GitHub identity resolution, team membership gating |
| Approver cache | members:read | Periodic approver team membership refresh |
| Scanner | contents:read, statuses:write | Config file reads, conflict commit statuses |

This scoping is not possible with PATs, which always carry their full scope.

## Monitoring rate limits

Watch for these log messages that indicate rate limit pressure:

- **Scanner:** `rate limit remaining below floor, pausing scan` -- the scanner's `rate_limit_floor` (default 500) was hit
- **Worker:** `GitHub secondary rate limit` -- concurrent request limit reached, retrying with backoff

If you see these frequently with PATs, switching to a GitHub App is the recommended next step.
