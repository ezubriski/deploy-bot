# Repo-Sourced App Discovery

## Overview

Allow applications to declare their deploy-bot configuration in their own
repositories. A collector goroutine in the receiver periodically scans
repositories in the configured GitHub org, fetches and validates each config
file, detects conflicts with operator-managed config, and writes the merged
result to a Kubernetes ConfigMap. The bot watches the projected file for
changes via its existing `config.Watch` mechanism.

## Repository Config File

Each repository may place a config file (name is configurable — defaults to
`.deploy-bot.json`) in the root of its default branch. The filename is
configurable because the bot may be installed under different names. A single
file can declare multiple apps (e.g. one per environment):

```json
{
  "apps": [
    {
      "app": "myapp",
      "environment": "dev",
      "kustomize_path": "apps/myapp/overlays/dev/kustomization.yaml",
      "ecr_repo": "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp",
      "tag_pattern": "^v\\d+\\.\\d+\\.\\d+$",
      "auto_deploy": true,
      "auto_deploy_approver_group": "C01234567"
    },
    {
      "app": "myapp",
      "environment": "prod",
      "kustomize_path": "apps/myapp/overlays/prod/kustomization.yaml",
      "ecr_repo": "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp",
      "tag_pattern": "^v\\d+\\.\\d+\\.\\d+$",
      "auto_deploy": false
    }
  ]
}
```

Fields mirror `AppConfig` in the bot's primary config. The collector validates
each entry against the same schema rules (e.g. `environment` is required,
`tag_pattern` compiles as a regex).

### Fields NOT settable from repos

Repo-sourced configs can only set per-app fields. They cannot modify global
settings (`github`, `slack`, `deployment`, `aws`, `ecr_events`). Any
unrecognised top-level keys are ignored with a warning.

## Precedence

**Operator config always wins.** If the operator's `config.json` defines an
app with the same `(app, environment)` pair as a repo-sourced entry, the
repo-sourced entry is discarded and a conflict warning is emitted. This is a
hard rule with no override mechanism — the operator must remove the entry from
their config for the repo-sourced version to take effect.

## Collector

The collector runs as a goroutine in the receiver process. It shares the
receiver's GitHub token and Slack client.

### Scan loop

```
startup → full scan → sleep(poll_interval) → full scan → ...
```

Each scan:

1. **List repos.** `Repositories.ListByOrg` with pagination. If
   `repo_prefix` is set, skip repos whose name does not start with the prefix.
2. **Fetch config file.** `Repositories.GetContents` for the config file path
   on the repo's default branch. Uses conditional requests (`If-None-Match`
   with the ETag from the previous scan) so unchanged files return 304 and
   don't count against the rate limit.
3. **Parse and validate.** Unmarshal JSON, validate each app entry (required
   fields, regex compilation, environment not empty). Invalid entries are
   logged and skipped — one bad entry does not invalidate the entire file.
4. **Conflict check.** For each `(app, environment)` pair, check whether it
   exists in the operator's primary config. Conflicts are collected for
   warning.
5. **Emit warnings.** See [Conflict Warnings](#conflict-warnings).
6. **Build merged config.** Start with the operator's `apps[]` list. Append
   all non-conflicting repo-sourced entries. The merged list is the final
   apps config.
7. **Update ConfigMap.** Serialise the merged config to JSON and patch the
   target ConfigMap. Only write if the content has changed (compare SHA-256
   of new vs current data key).

### Rate limit awareness

- Conditional requests (ETags) for file fetches — 304s are free.
- `ListByOrg` pages are cached; on subsequent scans only repos with a
  `pushed_at` newer than the last scan are re-fetched for their config file.
- If the remaining rate limit drops below a configurable floor (default 500),
  the scan sleeps until the reset window. This prevents the collector from
  starving the bot's own GitHub operations.

### Stale repo handling

If a repo that previously contributed apps is deleted, archived, or has its
config file removed, the collector notices on the next scan (404 on
`GetContents` or repo no longer listed). The apps from that repo are dropped
from the merged config, and the ConfigMap is updated. The bot sees the file
change and reloads — effectively removing those apps.

## Conflict Warnings

When the collector detects a conflict (repo-sourced app collides on
`(app, environment)` with operator config), it warns via two channels:

### Slack

Posts to the configured `warn_channel` (defaults to `deploy_channel`):

> App `myapp` (`prod`) is defined in both operator config and repo
> `my-org/myapp`. Remove it from operator config for the repo-sourced
> definition to take effect.

Warnings are **debounced**: the collector tracks which conflicts it has
already warned about and does not re-post until the conflict is resolved and
then reintroduced. This prevents noisy repeated messages on every scan cycle.

### GitHub (commit status)

The collector sets a **commit status** on the repo's default branch HEAD:

- **state:** `failure`
- **context:** `deploy-bot/config`
- **description:** `Conflict: myapp (prod) is defined in operator config`

When the conflict is resolved (operator removes the entry), the next scan
sets the status to `success`:

- **description:** `All apps registered successfully`

This gives repo owners visibility without requiring them to watch the Slack
deploy channel. Teams can optionally add `deploy-bot/config` as a required
status check to prevent merging config changes that won't take effect.

## ConfigMap Output

The collector writes to a ConfigMap separate from the operator's primary one.
The bot mounts both:

| ConfigMap | Mount path | Contents |
|---|---|---|
| `deploy-bot-config` (operator) | `/etc/deploy-bot/config.json` | Full config with global settings + operator-managed apps |
| `deploy-bot-discovered` (collector) | `/etc/deploy-bot/discovered.json` | `{"apps": [...]}` — only the repo-sourced apps list |

The bot's config loader merges both at load time: it reads the primary config,
then appends apps from the discovered file, skipping any `(app, environment)`
pairs already present in the primary. This merge happens on every reload
triggered by `config.Watch`.

### Why a separate ConfigMap?

- Operator config is never modified by automation — no risk of the collector
  clobbering manual changes.
- Clear ownership: operators own `deploy-bot-config`, the collector owns
  `deploy-bot-discovered`.
- If the collector is broken or disabled, the bot falls back to operator
  config only.

## Bot Config Changes

### Primary config (`config.json` top level)

```json
{
  "repo_discovery": {
    "enabled": true,
    "poll_interval": "5m",
    "config_file": ".deploy-bot.json",
    "repo_prefix": "",
    "discovered_path": "/etc/deploy-bot/discovered.json",
    "configmap_name": "deploy-bot-discovered",
    "configmap_namespace": "deploy-bot",
    "rate_limit_floor": 500,
    "warn_channel": ""
  }
}
```

| Field | Default | Description |
|---|---|---|
| `enabled` | `false` | Enable repo-sourced app discovery |
| `poll_interval` | `"5m"` | How often to scan repos |
| `config_file` | `".deploy-bot.json"` | Path in each repo to look for. Configurable because the bot may be installed under different names (e.g. `.shipit.json`, `.cd-bot.json`) |
| `repo_prefix` | `""` (all repos) | Only scan repos whose name starts with this prefix |
| `discovered_path` | `"/etc/deploy-bot/discovered.json"` | Where the bot reads the merged discovered apps |
| `configmap_name` | `"deploy-bot-discovered"` | ConfigMap to write discovered apps to |
| `configmap_namespace` | `""` (inferred from pod) | Namespace of the ConfigMap |
| `rate_limit_floor` | `500` | Pause scanning when remaining rate limit drops below this |
| `warn_channel` | `""` (uses `deploy_channel`) | Slack channel for conflict warnings |

### Discovered file format

```json
{
  "apps": [
    {
      "app": "myapp",
      "environment": "dev",
      "kustomize_path": "apps/myapp/overlays/dev/kustomization.yaml",
      "ecr_repo": "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp",
      "tag_pattern": "^v\\d+\\.\\d+\\.\\d+$",
      "auto_deploy": true,
      "_source_repo": "my-org/myapp"
    }
  ]
}
```

The `_source_repo` field is metadata for debugging and audit — it records
which repo contributed each entry. The bot's config loader ignores it.

## Bot Config Merge

`config.Load` gains an optional second path (`discoveredPath`). If the file
exists and is non-empty:

1. Parse it as `{"apps": [...]}`
2. Build a set of `(app, environment)` pairs from the primary config's apps
3. For each discovered app, skip if the pair exists in the primary set;
   otherwise append
4. Return the merged config

`config.Watch` watches both files. A change to either triggers a reload.

## New / Modified Packages

| Path | Change | Purpose |
|---|---|---|
| `internal/reposcanner/scanner.go` | New | Scan loop: list repos, fetch+parse config files, build discovered apps list |
| `internal/reposcanner/validate.go` | New | Validate repo-sourced app entries (required fields, regex, etc.) |
| `internal/reposcanner/configmap.go` | New | K8s ConfigMap patch logic (client-go) |
| `internal/reposcanner/conflict.go` | New | Conflict detection, Slack warning (debounced), GitHub commit status |
| `internal/config/config.go` | Modify | Add `RepoDiscovery` config struct; `Load` gains discovered-path merge |
| `internal/config/holder.go` | Modify | `Watch` monitors both primary and discovered paths |
| `cmd/receiver/main.go` | Modify | Start scanner goroutine when `repo_discovery.enabled` is true |

## Kubernetes RBAC

The receiver's ServiceAccount needs:

```yaml
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    resourceNames: ["deploy-bot-discovered"]
    verbs: ["get", "patch"]
```

Scoped to the single ConfigMap by `resourceNames`.

## Dependencies

- `k8s.io/client-go` — for ConfigMap patching. The receiver uses in-cluster
  config (`rest.InClusterConfig()`), so no kubeconfig is needed.

## Implementation Order

1. **Config additions** — `RepoDiscovery` struct on `Config`; `Load` gains
   discovered-path merge; `Watch` monitors both files.
2. **Validation** — `reposcanner/validate.go`: validate a parsed repo config
   against the same rules as `config.Load` (required fields, regex, etc.).
3. **Scanner core** — `reposcanner/scanner.go`: list repos, fetch files with
   ETags, parse JSON, filter by prefix, build discovered apps list.
4. **Conflict detection** — `reposcanner/conflict.go`: detect
   `(app, environment)` collisions with operator config; Slack warning with
   debounce; GitHub commit status.
5. **ConfigMap writer** — `reposcanner/configmap.go`: patch the target
   ConfigMap with the serialised discovered apps JSON. Skip if unchanged.
6. **Wire into receiver** — start scanner goroutine in `cmd/receiver/main.go`
   when enabled.
7. **Unit tests** — validation, conflict detection, merge logic, ETag caching.
8. **Integration test** — scanner against a test repo with known config; verify
   ConfigMap contents and conflict warnings.
