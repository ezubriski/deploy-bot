-- +goose Up
-- +goose StatementBegin
--
-- pending_deploys: in-flight deploys awaiting approval, merge, or
-- expiration.
--
-- Composite primary key (github_org, github_repo, pr_number) so the bot
-- can manage apps across multiple gitops repositories in a single
-- instance without PR numbers colliding. 1.x had an implicit single-repo
-- assumption baked into its `pending:<pr>` Redis key; removing it here
-- costs nothing today and future-proofs multi-repo tenancy without
-- requiring a later migration.
--
-- Row lifecycle mirrors the current Redis semantics:
--
--   INSERT with state='pending' on modal submission.
--   UPDATE state='merging' when the bot starts a merge on the GitHub
--     side (approve path).
--   UPDATE state='merged' when the merge completes.
--   DELETE on terminal events (approved/rejected/expired/cancelled).
--     The corresponding terminal-event record goes into the history
--     table; pending_deploys only holds rows for the in-flight window.
--
-- This matches the bot's existing Delete-on-transition flow. No
-- terminal-state rows linger here, so there's no retention pass
-- needed for pending_deploys (unlike history).
--
-- requested_at mirrors PendingDeploy.RequestedAt (the JSON field name)
-- so the 1.x -> 2.0 data migration can copy the value 1:1 without
-- renaming.
CREATE TABLE pending_deploys (
    github_org        TEXT         NOT NULL,
    github_repo       TEXT         NOT NULL,
    pr_number         INTEGER      NOT NULL,
    app               TEXT         NOT NULL,
    environment       TEXT         NOT NULL,
    tag               TEXT         NOT NULL,
    requester         TEXT         NOT NULL,
    requester_id      TEXT         NOT NULL,
    approver_id       TEXT,
    pr_url            TEXT         NOT NULL,
    slack_channel     TEXT,
    slack_message_ts  TEXT,
    reason            TEXT,
    requested_at      TIMESTAMPTZ  NOT NULL,
    expires_at        TIMESTAMPTZ  NOT NULL,
    state             TEXT         NOT NULL DEFAULT 'pending' CHECK (state IN (
                                     'pending', 'merging', 'merged'
                                  )),
    PRIMARY KEY (github_org, github_repo, pr_number)
);

-- Sweeper expiration scan: "give me in-flight rows ready to expire."
-- Partial index on state='pending' because rows in 'merging' or
-- 'merged' are mid-flight and shouldn't be expired by the sweeper —
-- those transitions have their own completion paths.
CREATE INDEX pending_deploys_expires_idx
    ON pending_deploys (expires_at)
    WHERE state = 'pending';

-- GetByEnvApp lookup: "is there an in-flight deploy for this app
-- already?" used when a new modal submission contends with a pending
-- one. Partial on state='pending' since 'merging'/'merged' rows are
-- irrelevant to the contention check.
CREATE INDEX pending_deploys_app_env_idx
    ON pending_deploys (app, environment)
    WHERE state = 'pending';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS pending_deploys;
-- +goose StatementEnd
