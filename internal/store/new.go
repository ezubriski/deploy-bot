package store

import (
	"context"
	"fmt"

	"github.com/ezubriski/deploy-bot/internal/config"
)

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
