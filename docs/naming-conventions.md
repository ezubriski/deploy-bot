# Naming conventions and conflict resolution

deploy-bot supports flexible app configuration, but flexibility introduces the possibility of conflicts — two apps targeting the same kustomization file, or duplicate names across operator and repo-sourced config. This document describes the naming conventions available, the conflicts that can occur, and how each is handled.

## Naming modes

### Flexible naming (default)

By default, app teams choose their own `app` name and `kustomize_path` in their `.deploy-bot.json`. This works well for small teams but requires coordination to avoid collisions.

**Example `.deploy-bot.json`** (v1 or v2):

```json
{
  "apiVersion": "deploy-bot/v1",
  "apps": [
    {
      "app": "my-service",
      "environment": "dev",
      "kustomize_path": "dev/my-service/kustomization.yaml",
      "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-service",
      "tag_pattern": "^v[0-9]+\\.[0-9]+\\.[0-9]+$"
    }
  ]
}
```

### Enforced repo naming (recommended)

When `enforce_repo_naming` is enabled in the bot's `config.json`, app names and kustomize paths are derived from the repository name. This eliminates naming collisions by convention.

**Bot config:**

```json
{
  "repo_discovery": {
    "enabled": true,
    "enforce_repo_naming": true
  }
}
```

**How names are derived:**

| Field | Convention | Example (repo: `org/my-service`, env: `dev`) |
|---|---|---|
| `app` | Repository name | `my-service` |
| `kustomize_path` | `<env>/<repo-name>/kustomization.yaml` | `dev/my-service/kustomization.yaml` |

**Example `.deploy-bot.json`** (v2 — `app` and `kustomize_path` omitted):

```json
{
  "apiVersion": "deploy-bot/v2",
  "apps": [
    {
      "environment": "dev",
      "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-service",
      "tag_pattern": "^v[0-9]+\\.[0-9]+\\.[0-9]+$"
    },
    {
      "environment": "prod",
      "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-service",
      "tag_pattern": "^v[0-9]+\\.[0-9]+\\.[0-9]+$"
    }
  ]
}
```

The scanner fills in:
- `app: "my-service"` (from the repo name)
- `kustomize_path: "dev/my-service/kustomization.yaml"` and `"prod/my-service/kustomization.yaml"`

If `app` or `kustomize_path` are specified explicitly, they must match the derived values. Mismatches are rejected:

```
✗ apps[0] (wrong-name): app: must be "my-service" when enforce_repo_naming
  is enabled (or omit to derive automatically)
```

**Validating locally:**

```bash
deploy-bot-config validate --file .deploy-bot.json --repo-naming --repo my-service
```

### API versions

| Version | `app` | `kustomize_path` | When to use |
|---|---|---|---|
| `deploy-bot/v1` | Required | Required | Flexible naming, or when `enforce_repo_naming` is off |
| `deploy-bot/v2` | Optional (derived when `enforce_repo_naming` is on) | Optional (derived when `enforce_repo_naming` is on) | Enforced naming. Falls back to v1 behavior when `enforce_repo_naming` is off |

v1 configs continue to work with `enforce_repo_naming` enabled, but `app` and `kustomize_path` must match the derived values (they cannot be omitted in v1).

## Conflict scenarios

### 1. Same `app` + `environment` in operator and repo config

**What happens:** The repo-sourced entry is excluded. The operator definition wins.

**Slack warning:**
> `my-service` (`dev`) from [org/my-service](...) — already defined in operator config. Remove the app from operator config to use the repo-sourced definition, or remove it from `.deploy-bot.json` to keep the operator definition.

**GitHub commit status:** Failure on the repo, naming the conflicting apps.

**How to fix:** Choose one source of truth. Either remove the app from operator `config.json` or remove it from the repo's `.deploy-bot.json`.

### 2. Same `kustomize_path` across operator and repo config

**What happens:** The repo-sourced entry is excluded. Two apps writing to the same kustomization file would overwrite each other's image tags.

**Slack warning:**
> `backend` (`dev`) from [org/backend](...) — `kustomize_path` conflicts with operator app frontend (dev). Each app must target a unique kustomization file. Update the `kustomize_path` in one of the configs to resolve this.

**How to fix:** Each app must target a unique kustomization file in the gitops repo. Update the `kustomize_path` in one of the configs. With `enforce_repo_naming` enabled, this cannot happen because paths are derived from repo names.

### 3. Same `kustomize_path` across two repo-sourced apps (different repos)

**What happens:** The second entry discovered is excluded. The first one wins (discovery order depends on GitHub API repo listing).

**Slack warning:** Same as scenario 2, but naming both repos.

**How to fix:** Same as scenario 2 — each app needs its own kustomization directory. With `enforce_repo_naming`, this is structurally impossible since each repo gets its own path.

### 4. Same `kustomize_path` within a single `.deploy-bot.json`

**What happens:** Caught by the validator (`deploy-bot-config validate --file`). The second entry is flagged as a conflict.

**How to fix:** Give each environment its own kustomization directory (e.g. `dev/my-service/` and `prod/my-service/`).

### 5. Same `app` + `environment` within a single `.deploy-bot.json`

**What happens:** Caught by the validator. The second entry is flagged as a duplicate.

**How to fix:** Remove the duplicate entry.

### 6. Operator config has two apps targeting the same `kustomize_path`

**What happens:** The bot refuses to start. This is a fatal error in `config.Load()`.

**How to fix:** Each app in operator config must target a unique kustomization file.

## Recommendation

Enable `enforce_repo_naming` for any deployment where multiple teams manage their own apps. The convention-based naming eliminates scenarios 2, 3, and 4 entirely, leaving only scenario 1 (operator override) as a possible conflict — which is intentional and clearly communicated.

For single-team setups where the operator manages all apps in `config.json`, repo-sourced discovery may not be needed. The `deploy-bot-config validate --config` command catches scenario 6 before deployment.
