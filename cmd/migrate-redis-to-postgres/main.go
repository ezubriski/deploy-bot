// migrate-redis-to-postgres is a one-shot tool for the 1.x → 2.0
// upgrade. It reads the existing deploy history list and pending
// deploy hashes from Redis and inserts them into the Postgres tables
// that the 2.0 bot reads from.
//
// Idempotent: re-running is safe. History rows use ON CONFLICT DO
// NOTHING on (event_type, app, environment, completed_at); pending
// rows use ON CONFLICT on the composite PK (github_org, github_repo,
// pr_number). A partial failure can be retried without duplicating
// rows.
//
// The tool reads CONFIG_PATH / SECRETS_PATH / AWS_SECRET_NAME exactly
// like the bot does, so operators don't need a separate credential
// path. github_org and github_repo are populated from the top-level
// github config section since 1.x is single-repo-by-definition.
//
// Usage:
//
//	CONFIG_PATH=/etc/deploy-bot/config.json \
//	SECRETS_PATH=/etc/deploy-bot/secrets.json \
//	./bin/migrate-redis-to-postgres
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/store"
	pgstore "github.com/ezubriski/deploy-bot/internal/store/postgres"
)

func main() {
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// ── Load config + secrets (same lookup as cmd/bot) ────────────
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		log.Fatal("CONFIG_PATH is required")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatal("load config", zap.Error(err))
	}

	var secrets *config.Secrets
	if sp := os.Getenv("SECRETS_PATH"); sp != "" {
		secrets, err = config.LoadSecretsFromFile(sp)
	} else if sn := os.Getenv("AWS_SECRET_NAME"); sn != "" {
		secrets, err = config.LoadSecrets(ctx, sn)
	} else {
		log.Fatal("set SECRETS_PATH or AWS_SECRET_NAME")
	}
	if err != nil {
		log.Fatal("load secrets", zap.Error(err))
	}
	if err := secrets.Validate(); err != nil {
		log.Fatal("invalid secrets", zap.Error(err))
	}

	org, repo := cfg.GitHub.Org, cfg.GitHub.Repo
	log.Info("using github org/repo for all migrated rows",
		zap.String("org", org),
		zap.String("repo", repo),
	)

	// ── Connect to Redis (source) ────────────────────────────────
	rdb := redis.NewClient(&redis.Options{
		Addr:     secrets.RedisAddr,
		Password: secrets.RedisToken,
	})
	defer func() {
		if err := rdb.Close(); err != nil {
			log.Warn("redis close", zap.Error(err))
		}
	}()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatal("redis ping", zap.Error(err))
	}
	log.Info("redis connected", zap.String("addr", secrets.RedisAddr))

	// ── Connect to Postgres (destination) ────────────────────────
	pgPool, err := pgstore.New(ctx, cfg.Postgres, secrets, log)
	if err != nil {
		log.Fatal("init postgres pool", zap.Error(err))
	}
	defer pgPool.Close()
	if err := pgPool.WaitFor(ctx, pgstore.DefaultWaitTimeout); err != nil {
		log.Fatal("postgres not available", zap.Error(err))
	}
	log.Info("postgres connected")

	// ── Migrate history ──────────────────────────────────────────
	historyVals, err := rdb.LRange(ctx, "history", 0, -1).Result()
	if err != nil {
		log.Fatal("redis LRANGE history", zap.Error(err))
	}
	log.Info("history entries in redis", zap.Int("count", len(historyVals)))

	var histOK, histSkip, histErr int
	for _, raw := range historyVals {
		var e store.HistoryEntry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			log.Warn("skip malformed history entry", zap.Error(err))
			histErr++
			continue
		}
		e.GitHubOrg = org
		e.GitHubRepo = repo

		// ON CONFLICT DO NOTHING: if a row with this
		// (event_type, app, environment, completed_at) already
		// exists, the INSERT is silently skipped. This makes the
		// tool idempotent — re-running after a partial failure
		// picks up where it left off.
		const q = `INSERT INTO history (
			github_org, github_repo, event_type, app, environment, tag,
			pr_number, pr_url, requester_id, approver_id, completed_at,
			gitops_commit_sha, slack_channel, slack_message_ts
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT DO NOTHING`
		tag, err := pgPool.Pool.Exec(ctx, q,
			e.GitHubOrg, e.GitHubRepo, e.EventType, e.App, e.Environment, e.Tag,
			e.PRNumber, e.PRURL, e.RequesterID, e.ApproverID, e.CompletedAt,
			e.GitopsCommitSHA, e.SlackChannel, e.SlackMessageTS,
		)
		if err != nil {
			log.Warn("history insert", zap.String("app", e.App), zap.Error(err))
			histErr++
			continue
		}
		if tag.RowsAffected() == 0 {
			histSkip++
		} else {
			histOK++
		}
	}
	log.Info("history migration complete",
		zap.Int("inserted", histOK),
		zap.Int("skipped_conflict", histSkip),
		zap.Int("errors", histErr),
	)

	// ── Migrate pending deploys ──────────────────────────────────
	keys, err := rdb.Keys(ctx, "pending:*").Result()
	if err != nil {
		log.Fatal("redis KEYS pending:*", zap.Error(err))
	}
	log.Info("pending keys in redis", zap.Int("count", len(keys)))

	var pendOK, pendSkip, pendErr int
	for _, key := range keys {
		raw, err := rdb.Get(ctx, key).Bytes()
		if err != nil {
			log.Warn("redis GET", zap.String("key", key), zap.Error(err))
			pendErr++
			continue
		}
		var d store.PendingDeploy
		if err := json.Unmarshal(raw, &d); err != nil {
			log.Warn("skip malformed pending entry", zap.String("key", key), zap.Error(err))
			pendErr++
			continue
		}
		d.GitHubOrg = org
		d.GitHubRepo = repo

		// Derive requester / requester_id — 1.x always has these.
		// Derive state — default to "pending" if missing.
		if d.State == "" {
			d.State = store.StatePending
		}
		if d.Requester == "" {
			d.Requester = d.RequesterID // best-effort fallback
		}

		const q = `INSERT INTO pending_deploys (
			github_org, github_repo, pr_number, app, environment, tag,
			requester, requester_id, approver_id, pr_url,
			slack_channel, slack_message_ts, reason,
			requested_at, expires_at, state
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (github_org, github_repo, pr_number) DO NOTHING`
		tag, err := pgPool.Pool.Exec(ctx, q,
			d.GitHubOrg, d.GitHubRepo, d.PRNumber, d.App, d.Environment, d.Tag,
			d.Requester, d.RequesterID, d.ApproverID, d.PRURL,
			d.SlackChannel, d.SlackMessageTS, d.Reason,
			d.RequestedAt, d.ExpiresAt, d.State,
		)
		if err != nil {
			// Check constraint violation on state: 1.x might have
			// states we don't allow (though unlikely). Log and skip.
			if strings.Contains(err.Error(), "violates check constraint") {
				log.Warn("pending insert: invalid state, skipping",
					zap.String("key", key),
					zap.String("state", d.State),
				)
				pendErr++
				continue
			}
			log.Warn("pending insert", zap.String("key", key), zap.Error(err))
			pendErr++
			continue
		}
		if tag.RowsAffected() == 0 {
			pendSkip++
		} else {
			pendOK++
		}
	}
	log.Info("pending migration complete",
		zap.Int("inserted", pendOK),
		zap.Int("skipped_conflict", pendSkip),
		zap.Int("errors", pendErr),
	)

	// ── Summary ──────────────────────────────────────────────────
	total := histOK + histSkip + histErr + pendOK + pendSkip + pendErr
	fmt.Fprintf(os.Stderr, "\n=== Migration Summary ===\n")
	fmt.Fprintf(os.Stderr, "History:  %d inserted, %d skipped (conflict), %d errors\n", histOK, histSkip, histErr)
	fmt.Fprintf(os.Stderr, "Pending:  %d inserted, %d skipped (conflict), %d errors\n", pendOK, pendSkip, pendErr)
	fmt.Fprintf(os.Stderr, "Total:    %d rows processed\n\n", total)

	if histErr+pendErr > 0 {
		fmt.Fprintf(os.Stderr, "Some rows failed — review the warnings above, fix, and re-run.\n")
		fmt.Fprintf(os.Stderr, "Re-running is safe (idempotent via ON CONFLICT DO NOTHING).\n")
		os.Exit(1)
	}
}
