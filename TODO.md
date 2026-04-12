# TODO

Deferred work and follow-ups. Items here are not blockers for any specific
release; they're tracked here so they don't get lost.

## Follow-up patches

- [ ] **CI-integrated perf snapshot.** Run a perf-snapshot job (capture
  per-host call counts and durations from `OTEL_METRICS_EXPORTER=console`
  on a known integration test such as `TestDeployAndApprove`) on every PR
  via GitHub Actions and post a comment with the delta vs `main`. Would
  catch GitHub/AWS call-count regressions in review instead of after
  merge. Use **GitHub Actions secrets**, not AWS Secrets Manager, for the
  integration creds — the integ harness already accepts a `SECRETS_PATH`
  alternative to `AWS_SECRET_NAME`, so the workflow can write a JSON file
  from one Actions secret and point `SECRETS_PATH` at it. Note that this
  creates real PRs against the test gitops repo on every CI run; budget
  for the resulting churn (or scope the job to a label like `perf-check`
  so it only runs on demand).

- [ ] **Investigate `TestMultiWorker_NoDoubleDelivery` flake.** Test passes on
  retry but intermittently times out waiting for a PR to appear in Redis when
  run back-to-back with `TestMultiWorker_LockContention`. Likely test
  pollution (Redis state from the prior multi-worker test). Add explicit
  cleanup between multi-worker tests or isolate them better.

- [ ] **Audit emission could be async.** Today every state transition (request,
  approve, reject, cancel, expire) blocks on `auditLog.Log` which writes to
  zap and optionally S3. The S3 path adds latency to the user-visible deploy
  flow. Consider a buffered channel + background flusher with bounded retries.

- [ ] **Collapse `appCache.tags` / `tagsWithTime` parallel slices.** The ECR
  cache currently stores filtered tags twice on `appCache`: once as
  `[]string` (`tags`) and once as `[]TagWithTime` (`tagsWithTime`), kept in
  lockstep by `filterRepoTags` and the two `Populate`/`refresh` write
  sites. Drop `tags` and derive it on demand from `tagsWithTime` (or
  rewrite `RecentTags`/`Tags` to read from the richer slice). Removes a
  drift hazard introduced by the tag-timestamp commit.

- [ ] **Alternative failure-signal source: AlertManager webhook receiver.**
  Today `on-health-degraded` is sourced exclusively from
  argocd-notifications, which is inherently susceptible to transient
  false positives: argocd rolls the app-level
  `.status.health.status` to `Degraded` for a reconcile tick during
  healthy RollingUpdates while per-resource health on the new
  revision is being recomputed. The current mitigation
  (`isTransientRolloutDegraded` in `internal/bot/argocd.go`) gates on
  deploy age + per-resource health emptiness — see the 2026-04-11
  homelab incident notes in chat/commit history and the Gotchas
  section in `docs/argocd-notifications.md`. That heuristic is a
  patch on top of a fundamentally noisy signal.

  The *correct* source of truth for "a Deployment is genuinely stuck"
  is `kube_deployment_status_condition{condition="Progressing",
  status="false", reason="ProgressDeadlineExceeded"} == 1` exposed by
  kube-state-metrics. An AlertManager webhook receiver on deploy-bot
  would:

  - Source from Prometheus, not argocd's async Lua health check, so
    no transient state window.
  - Work identically whether deploy-bot is in-cluster or remote —
    AlertManager just POSTs to a URL, same shape as
    argocd-notifications does now.
  - Decouple deploy-bot from argocd's notification pipeline, so a
    multi-cluster / multi-argocd deployment lives behind one
    webhook endpoint instead of requiring per-cluster
    argocd-notifications wiring.
  - Degrade gracefully: clusters that don't run Prometheus fall back
    to the argocd-notifications path (same as today).

  Required work: (1) new receiver endpoint at
  `/v1/webhooks/alertmanager` with AlertManager's payload shape, (2)
  new config section `alertmanager_notifications` + shared-secret
  field in secrets, (3) deploy-bot to stamp
  `deploy.bot/gitops-sha` and `deploy.bot/env` labels on Deployments
  at kustomize-patch time so AlertManager rules can join on them
  (via `kube_deployment_labels`) for correlation back to history,
  (4) worker dispatch that feeds the AlertManager alert into the
  same `postArgoCDFailure` rendering path, (5) docs + integration
  test.

  Tradeoffs: requires Prometheus + kube-state-metrics + AlertManager
  in every cluster deploy-bot is watching. Most prod clusters have
  this; the homelab currently does not. AlertManager is still
  cleaner than metric-backend-specific direct queries — it bridges
  Prometheus, VictoriaMetrics, Mimir, Cortex, etc. behind a common
  webhook protocol, insulating deploy-bot from per-vendor
  differences in PromQL dialects, auth schemes, and label support.

  **Recommended as the long-term preferred shape for failure
  notifications**, with the argocd-notifications `on-health-degraded`
  path kept as a fallback for clusters without Prometheus. Not
  urgent — the `isTransientRolloutDegraded` gate handles the
  observed failure mode for now.

- [ ] **Drop modernc/sqlite from the bot binary.** `internal/store/postgres`
  imports `github.com/pressly/goose/v3`, whose top-level package registers all
  dialects via blank imports — which transitively pulls in `modernc.org/sqlite`
  (a pure-Go sqlite implementation, ~7MB compiled) along with its supporting
  libraries (`modernc.org/libc`, `mathutil`, `memory`, `remyoudompheng/bigfft`,
  `dustin/go-humanize`, `mattn/go-isatty`, `ncruces/go-strftime`,
  `golang.org/x/exp`). We only use the postgres dialect; everything else is
  dead weight in the shipped binary. Fix: switch `Migrate()` from the global
  `goose.SetDialect("postgres")` + `goose.UpContext(db, "migrations")` API to
  `goose.NewProvider(database.DialectPostgres, sqlDB, migrationsFS)`, which
  registers only the requested dialect and lets the linker drop the rest. The
  refactor is local to `internal/store/postgres/postgres.go` and is a real
  change to how migrations are invoked (provider-scoped, not package-global),
  so it gets its own review pass rather than riding the postgres-3.0 branch.

- [ ] **Disable self-approval by default.** Self-approval (requester == approver)
  should be disabled by default; it's useful for testing but questionable in
  production. When disabled: (1) filter the requesting user out of the approver
  dropdown in the deploy modal, (2) if a user submits themselves as approver via
  slash command or other interface, respond with a message explaining why the
  request won't be fulfilled. Add a config flag (e.g.
  `deployment.allow_self_approval`, default `false`) so it can be enabled for
  test environments. Defense in depth: the worker's `handleApprove` path should
  also reject self-approvals (requester ID == approver ID on the pending deploy)
  with an error log and a message to the deploy channel, in case the UI-level
  guards are bypassed or a request is crafted outside the modal.

## Open questions

- Should `allow_prod_auto_deploy` move from a global guard to a per-app
  setting? Today an operator who wants auto-deploy in prod for one app must
  enable it globally. Per-app would be more granular but loses the global
  kill-switch property.

- The identity cache has no TTL. Stale entries persist forever after a user
  changes their email or GitHub username. Consider adding a TTL (say 24h) so
  identities re-resolve periodically.
