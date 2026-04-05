# Naming conventions and conflict resolution

deploy-bot supports flexible app configuration, but flexibility introduces the possibility of conflicts — two apps targeting the same kustomization file, or duplicate names across operator and repo-sourced config. This document describes the naming conventions available, the conflicts that can occur, and how each is handled.

We recommend enforcing strong conventions via `enforce_repo_naming` to avoid conflicts altogether. The bot can also function more flexibly for teams that need it.

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

When `enforce_repo_naming` is enabled, app names and kustomize paths are derived from the repository name. This eliminates naming collisions by convention.

**Bot config:**

```json
{
  "repo_discovery": {
    "enabled": true,
    "enforce_repo_naming": true,
    "kustomize_path_template": "{env}/{repo}/kustomization.yaml",
    "default_tag_pattern": "^v[0-9]+\\.[0-9]+\\.[0-9]+$",
    "exempt_repos": []
  }
}
```

| Setting | Description | Default |
|---|---|---|
| `enforce_repo_naming` | Require v2 configs and derive names from repo | `false` |
| `kustomize_path_template` | Path template with `{env}` and `{repo}` variables | `{env}/{repo}/kustomization.yaml` |
| `default_tag_pattern` | Tag pattern applied when an app omits `tag_pattern` | none |
| `exempt_repos` | Repos allowed to use v1 and bypass the convention | `[]` |

**How names are derived:**

| Field | Convention | Example (repo: `org/my-service`, env: `dev`) |
|---|---|---|
| `app` | Repository name | `my-service` |
| `kustomize_path` | From `kustomize_path_template` | `dev/my-service/kustomization.yaml` |
| `tag_pattern` | From `default_tag_pattern` (if omitted) | `^v[0-9]+\\.[0-9]+\\.[0-9]+$` |

**Example `.deploy-bot.json`** — minimal v2 config:

```json
{
  "apiVersion": "deploy-bot/v2",
  "apps": [
    {
      "environment": "dev",
      "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-service"
    },
    {
      "environment": "prod",
      "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-service"
    }
  ]
}
```

The scanner fills in:
- `app: "my-service"` (from the repo name)
- `kustomize_path` from the template (e.g. `dev/my-service/kustomization.yaml`)
- `tag_pattern` from `default_tag_pattern` (if the app doesn't specify one)

Apps can override `tag_pattern` per-entry if their tagging convention differs from the org default:

```json
{
  "apiVersion": "deploy-bot/v2",
  "apps": [
    {
      "environment": "dev",
      "ecr_repo": "123456789.dkr.ecr.us-east-1.amazonaws.com/my-service",
      "tag_pattern": "^release-.*$"
    }
  ]
}
```

If `app` or `kustomize_path` are specified explicitly, they must match the derived values. Mismatches are rejected:

```
✗ apps[0] (wrong-name): app: must be "my-service" when enforce_repo_naming
  is enabled (or omit to derive automatically)
```

### Custom path templates

The `kustomize_path_template` uses `{env}` and `{repo}` variables. Examples:

| Template | Result (`my-service`, `dev`) |
|---|---|
| `{env}/{repo}/kustomization.yaml` | `dev/my-service/kustomization.yaml` |
| `apps/{repo}/overlays/{env}/kustomization.yaml` | `apps/my-service/overlays/dev/kustomization.yaml` |
| `{repo}/{env}/kustomization.yaml` | `my-service/dev/kustomization.yaml` |
| `clusters/prod/{env}/{repo}/kustomization.yaml` | `clusters/prod/dev/my-service/kustomization.yaml` |

### Exempt repos

Some repos may need to break convention — for example, during a migration or when a legacy app has a non-standard directory structure. The operator can exempt specific repos:

```json
{
  "repo_discovery": {
    "enforce_repo_naming": true,
    "exempt_repos": ["org/legacy-app", "org/special-case"]
  }
}
```

Exempt repos can use `apiVersion: deploy-bot/v1` and specify any `app` name and `kustomize_path`. Non-exempt repos using v1 when enforcement is on are rejected:

```
✗ apiVersion: enforce_repo_naming requires apiVersion deploy-bot/v2.
  Update your config or contact the operator to add this repo to exempt_repos.
```

**Validating locally as an exempt repo:**

```bash
deploy-bot-config validate --file .deploy-bot.json --repo-naming --repo legacy-app --exempt
```

### API versions

| Version | `app` | `kustomize_path` | `tag_pattern` |
|---|---|---|---|
| `deploy-bot/v1` | Required | Required | Optional |
| `deploy-bot/v2` | Optional (derived) | Optional (derived) | Optional (defaults from operator) |

v2 configs work with or without `enforce_repo_naming`. When enforcement is off, v2 behaves like v1 (all fields required). When enforcement is on, omitted fields are derived from the repo name and operator template.

### Validating locally

```bash
# Minimal — v2 with enforcement
deploy-bot-config validate --file .deploy-bot.json \
  --repo-naming --repo my-service

# With custom template and default tag pattern
deploy-bot-config validate --file .deploy-bot.json \
  --repo-naming --repo my-service \
  --path-template "apps/{repo}/overlays/{env}/kustomization.yaml" \
  --default-tag-pattern "^v[0-9]+$"

# Exempt repo
deploy-bot-config validate --file .deploy-bot.json \
  --repo-naming --repo legacy-app --exempt

# Without enforcement (flexible mode)
deploy-bot-config validate --file .deploy-bot.json
```

## Conflict scenarios

### 1. Same `app` + `environment` in operator and repo config

**What happens:** The repo-sourced entry is excluded. The operator definition wins.

**Slack warning:**
> `my-service` (`dev`) from [org/my-service](...) — already defined in operator config. Remove the app from operator config to use the repo-sourced definition, or remove it from `.deploy-bot.json` to keep the operator definition.

**GitHub commit status:** Failure on the repo, naming the conflicting apps.

**How to fix:** Choose one source of truth. Either remove the app from operator `config.json` or remove it from the repo's `.deploy-bot.json`.

**Prevention:** This conflict is intentional — operator config always takes precedence. Use it when you need to override a repo-sourced definition.

### 2. Same `kustomize_path` across operator and repo config

**What happens:** The repo-sourced entry is excluded. Two apps writing to the same kustomization file would overwrite each other's image tags.

**Slack warning:**
> `backend` (`dev`) from [org/backend](...) — `kustomize_path` conflicts with operator app frontend (dev). Each app must target a unique kustomization file. Update the `kustomize_path` in one of the configs to resolve this.

**How to fix:** Each app must target a unique kustomization file in the gitops repo. Update the `kustomize_path` in one of the configs.

**Prevention:** Enable `enforce_repo_naming`. Each repo gets its own path derived from the template, making this structurally impossible.

### 3. Same `kustomize_path` across two repo-sourced apps (different repos)

**What happens:** The second entry discovered is excluded. The first one wins (discovery order depends on GitHub API repo listing).

**Slack warning:** Same as scenario 2, but naming both repos.

**How to fix:** Same as scenario 2 — each app needs its own kustomization directory.

**Prevention:** Enable `enforce_repo_naming`. Different repos always produce different paths.

### 4. Same `kustomize_path` within a single `.deploy-bot.json`

**What happens:** Caught by the validator (`deploy-bot-config validate --file`). The second entry is flagged as a conflict.

**How to fix:** Give each environment its own kustomization directory.

**Prevention:** With `enforce_repo_naming`, paths include the environment (e.g. `dev/my-service/` and `prod/my-service/`), so this cannot happen.

### 5. Same `app` + `environment` within a single `.deploy-bot.json`

**What happens:** Caught by the validator. The second entry is flagged as a duplicate.

**How to fix:** Remove the duplicate entry.

### 6. Operator config has two apps targeting the same `kustomize_path`

**What happens:** The bot refuses to start. This is a fatal error in `config.Load()`.

**How to fix:** Each app in operator config must target a unique kustomization file. Run `deploy-bot-config validate --config config.json` to catch this before deploying.

## Recommendation

For multi-team organizations, enable `enforce_repo_naming` with a `kustomize_path_template` matching your gitops repo structure and a `default_tag_pattern` matching your tagging convention. This eliminates scenarios 2, 3, and 4 entirely. Use `exempt_repos` sparingly for teams migrating to the convention.

For single-team setups where the operator manages all apps in `config.json`, repo-sourced discovery may not be needed. The `deploy-bot-config validate --config` command catches scenario 6 before deployment.
