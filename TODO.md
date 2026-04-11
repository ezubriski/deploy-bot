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
