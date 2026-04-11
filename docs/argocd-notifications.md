# ArgoCD Notifications Integration

## Overview

deploy-bot can optionally subscribe to lifecycle notifications from
[Argo CD's notifications controller](https://argo-cd.readthedocs.io/en/stable/operator-manual/notifications/)
for the apps it manages. The goal is **not** to be a generic ArgoCD message
forwarder — it's to surface deploy outcomes that should change human
behaviour, and to suggest actions (rollback) where the bot can help.

The current scope:

| Argo trigger          | What the bot does                                                                  |
| --------------------- | ---------------------------------------------------------------------------------- |
| `on-sync-succeeded`   | Quiet threaded reply under the original deploy message: "synced and healthy"       |
| `on-sync-failed`      | **Top-level alarming message** with failing-resource detail, plus a separate rollback-prompt message with `[Roll back]` / `[Dismiss]` buttons |
| `on-health-degraded`  | Same as `on-sync-failed`                                                           |
| `on-sync-running`     | _Subscribed but currently dropped._ See "Future work" below                        |

> **Phase 4 status (this commit):** alongside every phase-3 failure status
> message, the bot now posts a **separate top-level "suggested action"
> message** carrying `[Roll back]` and `[Dismiss]` buttons. The rollback
> target is the previous known-good tag for the same app/env, resolved
> from deploy history at post time and baked into the button payload so
> clicks stay deterministic. Late-arriving failures (deploy > 2h old) and
> first-ever deploys (no prior approved history entry) suppress the
> prompt entirely — the status message still lands.
>
> **Buttons:**
>
> - `[Roll back]` opens the standard deploy approval modal in rollback
>   mode with the app, environment, and previous tag pre-filled. The
>   submission goes through the normal approval flow, so the usual
>   per-app lock, tag validation, and approver authorization apply.
> - `[Dismiss]` replaces the prompt buttons in place with a
>   ":no_entry_sign: *Dismissed* by @user" line. Only members of the
>   authorized team can dismiss — other users get an ephemeral refusal
>   and the prompt stays clickable.

## Architecture

```
ArgoCD app  ─►  argocd-notifications-controller  ─►  POST /v1/webhooks/argocd
                                                              │
                                                              ▼
                                                       receiver (auth,
                                                       parse, dedupe)
                                                              │
                                                              ▼
                                                   argocd:events Redis stream
                                                              │
                                                              ▼
                                                   worker (correlate by
                                                   gitops SHA, post Slack)
```

The receiver endpoint is mounted on the existing health-server mux at
`:8080/v1/webhooks/argocd`. It mirrors the ECR webhook in shape: shared-secret
header auth, 1 MiB body limit, fail-closed on Redis dedupe errors, fail-soft
on stream-write errors (the in-memory buffer absorbs them).

### Correlation

ArgoCD's notification carries the **gitops repo commit SHA**
(`.app.status.operationState.syncResult.revision`). deploy-bot persists the
same SHA on every `HistoryEntry` (since #47 / phase 1) when it merges a deploy
PR. The worker matches on that SHA via `store.FindHistoryBySHA` to find the
originating deploy and the Slack message it posted, so follow-up
notifications can post in the right context.

No annotation or naming convention on the ArgoCD `Application` is required —
correlation is purely by SHA. If a notification arrives for a SHA the bot has
no history entry for (e.g. a deploy made via another tool, or an entry that
has aged out of the 100-entry retained window), it is logged at info level
and dropped. The whole value proposition of the handler is "deploy-bot
knows who deployed what" — without a matching history entry there's nothing
actionable to say.

### Late-arriving notifications

If the matched history entry's `CompletedAt` is more than **2 hours** ago
when a `sync-failed` or `health-degraded` notification arrives, the handler
re-frames the message: no siren emojis, no ALL-CAPS banner, and an
":information_source: _This deploy is more than 2 hours old. The current
failure may not be caused by this deploy — investigate before rolling back._"
note. The rationale (from the planning discussion): a sync that was healthy
for hours and then degrades is almost certainly a runtime issue, not a
bad deploy, and rolling back a hours-old stable deploy is rarely the right
first response.

### Rollback prompt (phase 4)

The rollback target is resolved from deploy history at prompt-post time,
not at click time. The handler scans history newest-first for the most
recent `approved` entry for the same composite `app-env` FullName whose
`CompletedAt` is **strictly before** the failing deploy's `CompletedAt`,
and embeds the resulting tag in the button's JSON payload. This handles
the race where a newer deploy has already completed by the time an
ArgoCD notification arrives: we still want to roll back past the
failing deploy, not past an unrelated newer one.

Suppression conditions (logged at info level, no prompt posted):

- **Late arrival** (failing deploy > `lateArrivalThreshold`, default 2h).
  A hours-old deploy that has been healthy for hours is almost never
  the right thing to roll back.
- **No prior approved history entry** for this app/env. This is the
  first-ever deploy, so there is no known-good tag to offer.
- **History lookup failure**. Redis blip during the notification
  handler — the status message has already posted, and dropping the
  prompt is safer than offering a `[Roll back]` button that can't
  resolve its target.

Authorization:

- **`[Roll back]`** requires the clicker to be a member of the
  authorized team (same bar as `/deploy rollback`). On click, the
  standard deploy modal opens in rollback mode — the subsequent
  submit, approval, and merge all go through the normal flow.
- **`[Dismiss]`** also requires team membership. On click the prompt
  message is updated in place: buttons are replaced with a
  ":no_entry_sign: *Dismissed* by @user" line. The underlying failure
  status message is left untouched.

### Resource detail

The webhook template includes `.app.status.resources` as a JSON array in the
payload. On `sync-failed` / `health-degraded` the handler parses this,
filters to entries with a non-empty non-`Healthy` `healthStatus`, and
renders the first 10 as a bullet list in the alert. Resources with no
health status (ConfigMaps, Secrets, Services) are filtered out — they
don't report health and would just be visual noise. Overflow beyond 10
renders as "_…and N more_".

### Dedupe

Each `(argocd_app, gitops_sha, trigger)` tuple is deduped through a Redis
`SET NX` with a 24h TTL. This handles three sources of duplication:

1. **ArgoCD controller restart**: notification state is in-process, so a
   pod restart can replay recent events.
2. **Our own at-least-once stream semantics**: `XAUTOCLAIM` reclaims stuck
   messages and would otherwise re-deliver them.
3. **Multi-replica receivers**: a future HA receiver pair would otherwise
   double-process if the upstream balanced them.

A 24h marker is long enough to absorb all three without growing the dedupe
key set unboundedly (the entries naturally TTL out).

## Configuration

### `config.json`

```json
{
  "argocd_notifications": {
    "enabled": false
  }
}
```

That's the entire config surface for now. When `enabled` is `false` (the
default), the receiver mounts no handler and consumes no Redis stream — the
behaviour is identical to a standard install.

### Secrets

Add to the receiver's secrets file (or AWS secret) when enabling the feature:

```json
{
  "argocd_webhook_api_key": "<32+ character random string>"
}
```

The receiver fails fast at startup if the key is shorter than 32 characters
when `argocd_notifications.enabled` is `true`.

## ArgoCD-side setup

The receiver expects the
[`webhook` service](https://argo-cd.readthedocs.io/en/stable/operator-manual/notifications/services/webhook/)
in argocd-notifications, configured to POST a JSON body in deploy-bot's
expected shape with the shared secret in the `X-Deploybot-Secret` header.

A reference `ConfigMap`/`Secret` patch is committed at
[`deploy/argocd-notifications/templates.yaml`](../deploy/argocd-notifications/templates.yaml).
Apply it to your `argocd` namespace alongside the rest of the
`argocd-notifications-cm` config. It defines:

- A custom `service.webhook.deploybot` pointing at the receiver URL with the
  `X-Deploybot-Secret` header sourced from `argocd-notifications-secret`.
- A custom `template.deploybot-event` whose `body:` renders deploy-bot's
  expected JSON shape (see below).
- A wiring section showing how to add the new template to each of the four
  triggers we care about.

### Subscribing an Application

On any `Application` you want deploy-bot to receive notifications for, add:

```yaml
metadata:
  annotations:
    notifications.argoproj.io/subscribe.on-sync-succeeded.deploybot: ""
    notifications.argoproj.io/subscribe.on-sync-failed.deploybot: ""
    notifications.argoproj.io/subscribe.on-health-degraded.deploybot: ""
```

The `deploybot` segment matches the `service.webhook.deploybot` name in the
ConfigMap. No deploy-bot–specific annotations are needed.

You can also subscribe at the `AppProject` level to apply to every
Application in the project, or use the global `subscriptions:` selector form
in `argocd-notifications-cm` to label-select multiple apps at once.

## Webhook payload shape

The full payload schema is in
[`internal/argocd/payload.go`](../internal/argocd/payload.go) — the receiver
ignores unknown fields, so additive changes to the upstream template are
backwards compatible. The minimum a recognized payload must carry is:

```json
{
  "trigger": "on-sync-succeeded",
  "argocdApp": "myapp-prod",
  "syncResultRevision": "abc123def456..."
}
```

A degraded notification with the resource detail block looks like:

```json
{
  "trigger": "on-health-degraded",
  "argocdApp": "myapp-prod",
  "namespace": "argocd",
  "repoURL": "https://github.com/org/gitops",
  "syncResultRevision": "abc123def456...",
  "syncStatus": "Synced",
  "healthStatus": "Degraded",
  "phase": "Succeeded",
  "message": "Health check failed",
  "finishedAt": "2026-04-11T12:34:56Z",
  "resources": [
    {
      "kind": "Deployment",
      "name": "myapp",
      "namespace": "default",
      "syncStatus": "Synced",
      "healthStatus": "Degraded",
      "healthMessage": "ReplicaSet \"myapp-7d4...\" has timed out progressing"
    }
  ]
}
```

The `resources` array is preserved verbatim through the receiver — phase 3
will render it on degraded/failed messages without re-parsing in the
correlation layer.

## Compatibility

The reference template is developed against **Argo CD v3.3.6** (the current
stable release). The default trigger catalog (`on-sync-succeeded`,
`on-sync-failed`, `on-health-degraded`, `on-sync-running`) has been stable
since early v2.x, so the wiring should work on older versions. Confirmation
against older releases will land once the homelab cluster upgrades.

## Future work

- **`on-sync-running` watchdog**: rather than echoing every "sync started"
  notification (low signal), use the absence of a `sync-succeeded` within
  N minutes of a merge to detect stuck ArgoCD or unsubscribed apps. Tracked
  separately from this feature.
- **Recovery detection**: post a green "recovered to healthy" reply under
  the original failure when a degraded app self-heals. Requires a custom
  `on-app-healthy` trigger whose `oncePer` semantics need empirical
  verification against v3.3.6 — gated on the homelab cluster upgrade.

## Gotchas

- **Delivery is best-effort.** ArgoCD's webhook service retries 3x on 5xx
  and network errors only. 4xx is **not** retried, and there is no DLQ.
  Return 2xx fast and absorb infrastructure failures internally — that is
  why this handler returns 200 even when it dropped a notification as
  unrecognized or deduped.
- **Controller in-memory state.** Notification state lives in the
  argocd-notifications controller process. A pod restart can re-deliver
  recent events. The receiver dedupe window (24h) is sized to cover this.
- **Annotation typos are silent.** If `notifications.argoproj.io/subscribe.<trigger>.deploybot`
  doesn't match the `service.webhook.<name>` in the ConfigMap exactly, the
  controller silently delivers nothing. Check
  `kubectl logs deploy/argocd-notifications-controller` for `notification
  service ... not found` if you expected events and aren't seeing them.
- **No HMAC.** The argocd-notifications template engine cannot compute HMAC,
  so authentication is shared-secret-in-header. Terminate at an internal
  ingress that adds mTLS if you need stronger transport security.
