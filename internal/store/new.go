package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ezubriski/deploy-bot/internal/config"
)

// NewPostgresOnly builds a Store backed solely by Postgres, with no Redis
// client attached. Used by read-only consumers (e.g. cmd/api) that need
// history/pending queries but have no business touching locks, streams,
// or thread timestamps. Any method that depends on Redis will panic —
// the nil checks in each method make the misuse loud rather than silent.
func NewPostgresOnly(pg *pgxpool.Pool) *Store {
	return &Store{pg: pg}
}

// NewFromSecrets creates a Store from the loaded secrets configuration,
// automatically selecting password or ElastiCache IAM authentication.
func NewFromSecrets(ctx context.Context, secrets *config.Secrets) (*Store, error) {
	if !secrets.RedisIAMAuth {
		return New(secrets.RedisAddr, secrets.RedisToken), nil
	}

	credProvider, err := IAMCredentialsProvider(ctx, secrets.RedisUserID, secrets.RedisReplicationGroupID)
	if err != nil {
		return nil, fmt.Errorf("init elasticache IAM auth: %w", err)
	}

	return NewWithOptions(Options{
		Addr:                secrets.RedisAddr,
		IAMAuth:             true,
		CredentialsProvider: credProvider,
	}), nil
}
