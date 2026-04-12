-- +goose Up
-- +goose StatementBegin
--
-- history: completed deploy events (approved, rejected, expired, cancelled).
--
-- One row per transition into a terminal state. Append-mostly; read by
-- three hot paths:
--
--   1. `/deploy history [app-env]`         -> (app, environment, completed_at DESC) scan
--   2. Rollback target resolution (phase 4) -> (app, environment, completed_at DESC) scan
--                                              filtered to event_type='approved'
--   3. ArgoCD notification correlation      -> FindHistoryBySHA, one-row lookup
--                                              on gitops_commit_sha
--
-- All three are backed by indexes defined below.
--
-- github_org and github_repo are populated for every new row in 2.0+ but
-- nullable for the benefit of the 1.x -> 2.0 data migration (which fills
-- them in from the bot's top-level github config since 1.x is
-- single-repo-by-definition). A later migration can tighten the NOT NULL
-- constraint once the 1.x upgrade window has passed for all operators.
CREATE TABLE history (
    id                  BIGSERIAL    PRIMARY KEY,
    event_type          TEXT         NOT NULL CHECK (event_type IN (
                                        'approved', 'rejected', 'expired', 'cancelled'
                                    )),
    app                 TEXT         NOT NULL,
    environment         TEXT         NOT NULL,
    tag                 TEXT         NOT NULL,
    pr_number           INTEGER,
    pr_url              TEXT,
    github_org          TEXT,
    github_repo         TEXT,
    requester_id        TEXT         NOT NULL,
    approver_id         TEXT,
    completed_at        TIMESTAMPTZ  NOT NULL,
    gitops_commit_sha   TEXT,
    slack_channel       TEXT,
    slack_message_ts    TEXT,
    inserted_at         TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- `/deploy history app-env` and rollback target resolution.
-- DESC on completed_at so the most common query (newest-first per app)
-- reads directly from the index without a sort step.
CREATE INDEX history_app_env_completed_idx
    ON history (app, environment, completed_at DESC);

-- ArgoCD notification correlation. Partial index: rows without a merge
-- SHA (rejected / expired / cancelled) are never queried this way, so
-- we exclude them to keep the index small.
CREATE INDEX history_gitops_sha_idx
    ON history (gitops_commit_sha)
    WHERE gitops_commit_sha IS NOT NULL;

-- Retention ticker's DELETE predicate. Unconditional index on
-- completed_at is cheap and also serves the "newest globally" query
-- used by `/deploy history` when no app filter is supplied.
CREATE INDEX history_completed_at_idx
    ON history (completed_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
--
-- 2.0 does not support downgrading past this migration; operators
-- rolling back to 1.x must restore from a pre-2.0 Redis dump. This
-- down block exists so goose is happy, not because it's exercised in
-- any supported workflow.
DROP TABLE IF EXISTS history;
-- +goose StatementEnd
