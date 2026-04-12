package postgres

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ezubriski/deploy-bot/internal/config"
)

// installIAMAuthHook configures the pgxpool.Config to obtain a fresh
// RDS IAM auth token on every new physical connection. The token is
// short-lived (~15m) and the pool may hold connections for longer,
// so refreshing at connect time keeps things simple without needing
// a background token-refresh loop: old connections with stale-but-
// still-valid tokens keep working until the pool recycles them, and
// new connections always get a fresh token.
//
// This mirrors the existing ElastiCache Redis IAM pattern in
// internal/store/elasticache_iam.go. The main differences are (a)
// RDS uses a dedicated helper package from aws-sdk-go-v2
// (feature/rds/auth) that handles the SigV4 signing internally, so
// we don't need to build the presigned URL ourselves, and (b) the
// token is passed as the Postgres "password" field rather than as
// a separate Redis AUTH credential.
func installIAMAuthHook(poolCfg *pgxpool.Config, cfg config.PostgresConfig, secrets *config.Secrets) error {
	if secrets.PostgresRDSRegion == "" {
		return fmt.Errorf("postgres_rds_region is required for RDS IAM auth (should have been caught by Secrets.Validate)")
	}

	region := secrets.PostgresRDSRegion
	host := cfg.Host
	port := cfg.PortValue()
	user := cfg.User
	endpoint := fmt.Sprintf("%s:%d", host, port)

	// Load the default AWS credentials chain once. Reused across
	// every BeforeConnect call — same pattern as the ElastiCache
	// path, where creds are cached and only the presigning runs
	// on each token refresh.
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
	)
	if err != nil {
		return fmt.Errorf("load aws config: %w", err)
	}

	poolCfg.BeforeConnect = func(ctx context.Context, connCfg *pgx.ConnConfig) error {
		token, err := auth.BuildAuthToken(ctx, endpoint, region, user, awsCfg.Credentials)
		if err != nil {
			return fmt.Errorf("build rds auth token: %w", err)
		}
		connCfg.Password = token
		return nil
	}
	return nil
}
