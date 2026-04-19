// Command api is a read-only HTTP service over deploy-bot's Postgres
// state, intended for consumption by a UI running in the same
// Kubernetes cluster. It reuses internal/config, internal/store, and
// internal/store/postgres from the bot so schema changes flow through
// one package boundary.
//
// The API does not run migrations, touch Redis, or connect to Slack,
// GitHub, or ECR. It only needs a Postgres role with SELECT on
// `history` and `pending_deploys`; operators should provision that
// role separately from the bot's read/write user.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/api"
	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/health"
	"github.com/ezubriski/deploy-bot/internal/observability"
	"github.com/ezubriski/deploy-bot/internal/store"
	pgstore "github.com/ezubriski/deploy-bot/internal/store/postgres"
)

const (
	defaultAPIAddr     = ":8080"
	defaultMetricsAddr = ":9091"
	shutdownTimeout    = 10 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "/etc/deploy-bot/config.json"
	}
	primaryCfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	level, err := config.ResolvedLogLevel(primaryCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve log level: %v\n", err)
		os.Exit(1)
	}
	format, err := config.ResolvedLogFormat(primaryCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve log format: %v\n", err)
		os.Exit(1)
	}
	log, err := config.NewLogger(level, format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := log.Sync(); err != nil {
			fmt.Fprintf(os.Stderr, "logger sync: %v\n", err)
		}
	}()

	var discoveredPath string
	if primaryCfg.RepoDiscovery.Enabled {
		discoveredPath = primaryCfg.RepoDiscovery.DiscoveredFilePath()
	}
	initialCfg, err := config.LoadWithDiscovered(configPath, discoveredPath)
	if err != nil {
		log.Fatal("load config with discovered", zap.Error(err))
	}
	cfgHolder := config.NewHolderWithDiscovered(initialCfg, configPath, discoveredPath)

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

	obsProvider, err := observability.Setup("deploy-bot-api", prometheus.DefaultRegisterer)
	if err != nil {
		log.Fatal("init observability", zap.Error(err))
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := obsProvider.Shutdown(shutdownCtx); err != nil {
			log.Warn("observability shutdown", zap.Error(err))
		}
	}()

	pgPool, err := pgstore.New(ctx, initialCfg.Postgres, secrets, log)
	if err != nil {
		log.Fatal("init postgres pool", zap.Error(err))
	}
	defer pgPool.Close()
	log.Info("waiting for postgres",
		zap.String("host", initialCfg.Postgres.Host),
		zap.Int("port", initialCfg.Postgres.PortValue()),
		zap.String("database", initialCfg.Postgres.Database),
	)
	if err := pgPool.WaitFor(ctx, pgstore.DefaultWaitTimeout); err != nil {
		log.Fatal("postgres not available", zap.Error(err))
	}
	log.Info("postgres connected")

	reader := store.NewPostgresOnly(pgPool.Pool)
	server := api.New(reader, cfgHolder, log)

	oidcCfg := api.OIDCConfig{
		IssuerURL: os.Getenv("OIDC_ISSUER_URL"),
		Audience:  os.Getenv("OIDC_AUDIENCE"),
	}
	if oidcCfg.IssuerURL == "" || oidcCfg.Audience == "" {
		log.Fatal("OIDC_ISSUER_URL and OIDC_AUDIENCE must be set")
	}
	authMW, err := api.OIDC(ctx, oidcCfg, log)
	if err != nil {
		log.Fatal("init oidc", zap.Error(err))
	}

	hh := &health.Handler{}
	hh.SetHealthy()
	hh.SetCacheReady()

	adminMux := http.NewServeMux()
	adminMux.Handle("/metrics", promhttp.Handler())
	adminMux.HandleFunc("/healthz", hh.Liveness)
	adminMux.HandleFunc("/readyz", hh.Readiness)
	adminSrv := &http.Server{
		Addr:              getenv("METRICS_ADDR", defaultMetricsAddr),
		Handler:           adminMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("admin server listening", zap.String("addr", adminSrv.Addr))
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("admin server error", zap.Error(err))
		}
	}()

	apiSrv := &http.Server{
		Addr:              getenv("API_ADDR", defaultAPIAddr),
		Handler:           authMW(server.Routes()),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("api server listening", zap.String("addr", apiSrv.Addr))
		if err := apiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("api server error", zap.Error(err))
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := apiSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("api server shutdown", zap.Error(err))
	}
	if err := adminSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("admin server shutdown", zap.Error(err))
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
