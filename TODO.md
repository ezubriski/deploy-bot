# TODO

Deferred work and follow-ups. Items here are not blockers for any specific
release; they're tracked here so they don't get lost.

## Follow-up patches

- [ ] **Investigate `TestMultiWorker_NoDoubleDelivery` flake.** Test passes on
  retry but intermittently times out waiting for a PR to appear in Redis when
  run back-to-back with `TestMultiWorker_LockContention`. Likely test
  pollution (Redis state from the prior multi-worker test). Add explicit
  cleanup between multi-worker tests or isolate them better.

- [ ] **Audit emission could be async.** Today every state transition (request,
  approve, reject, cancel, expire) blocks on `auditLog.Log` which writes to
  zap and optionally S3. The S3 path adds latency to the user-visible deploy
  flow. Consider a buffered channel + background flusher with bounded retries.

## Open questions

- Should `allow_prod_auto_deploy` move from a global guard to a per-app
  setting? Today an operator who wants auto-deploy in prod for one app must
  enable it globally. Per-app would be more granular but loses the global
  kill-switch property.

- The identity cache has no TTL. Stale entries persist forever after a user
  changes their email or GitHub username. Consider adding a TTL (say 24h) so
  identities re-resolve periodically.
