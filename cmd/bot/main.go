package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"fmt"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/slack-go/slack"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/bot"
	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/ecr"
	githubPkg "github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/health"
	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/store"
	"github.com/ezubriski/deploy-bot/internal/sweeper"
	"github.com/ezubriski/deploy-bot/internal/validator"
)

const (
	metricsAddr    = ":9090"
	sweepLockTTL   = 6 * time.Minute // slightly longer than sweepInterval
)

func main() {
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "/etc/deploy-bot/config.json"
	}
	initialCfg, err := config.Load(configPath)
	if err != nil {
		log.Fatal("load config", zap.Error(err))
	}
	cfgHolder := config.NewHolder(initialCfg, configPath)

	secrets, err := config.LoadSecrets(ctx, os.Getenv("AWS_SECRET_NAME"))
	if err != nil {
		log.Fatal("load secrets", zap.Error(err))
	}
	if err := secrets.Validate(); err != nil {
		log.Fatal("invalid secrets", zap.Error(err))
	}

	m := metrics.NewDefault()
	hh := &health.Handler{}
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/healthz", hh.Liveness)
		mux.HandleFunc("/readyz", hh.Readiness)
		log.Info("metrics/health server listening", zap.String("addr", metricsAddr))
		if err := http.ListenAndServe(metricsAddr, mux); err != nil {
			log.Error("metrics/health server error", zap.Error(err))
		}
	}()

	redisStore := store.New(secrets.RedisAddr, secrets.RedisToken)
	if err := redisStore.Ping(ctx); err != nil {
		log.Fatal("redis ping", zap.Error(err))
	}

	ghClient := githubPkg.NewClient(secrets.GitHubToken, cfgHolder.Load().GitHub.Org, cfgHolder.Load().GitHub.Repo)

	slackClient := slack.New(secrets.SlackBotToken,
		slack.OptionAppLevelToken(secrets.SlackAppToken),
	)

	ecrCache, err := ecr.NewCache(ctx, cfgHolder.Load(), m, log)
	if err != nil {
		log.Fatal("init ecr cache", zap.Error(err))
	}

	auditLog, err := audit.NewLogger(ctx, cfgHolder.Load(), log)
	if err != nil {
		log.Fatal("init audit logger", zap.Error(err))
	}

	val := validator.New(secrets.GitHubToken, slackClient, cfgHolder.Load(), log)

	b := bot.New(slackClient, redisStore, ghClient, ecrCache, val, auditLog, m, cfgHolder, log)
	sw := sweeper.New(redisStore, ghClient, slackClient, auditLog, m, cfgHolder, log)

	config.Watch(ctx, cfgHolder, 30*time.Second, func(newCfg *config.Config) {
		if err := ecrCache.AddApps(newCfg.Apps); err != nil {
			log.Warn("ecr cache: failed to register new apps after reload", zap.Error(err))
		}
	}, log)

	// Populate ECR cache; mark ready once done.
	ecrCache.Populate(ctx)
	hh.SetCacheReady()
	ecrCache.StartRefresh(ctx)

	// Recover any deploys stuck in merging state from a previous crash.
	sw.RecoverStuck(ctx)

	// Start the sweeper with a Redis lock so only one worker pod runs it.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				acquired, err := redisStore.TryLock(ctx, "sweeper", sweepLockTTL)
				if err != nil {
					log.Error("sweeper lock", zap.Error(err))
					continue
				}
				if acquired {
					sw.RunOnce(ctx)
				}
			}
		}
	}()

	// Initialise the consumer group before starting the worker loop.
	qw := queue.NewWorker(redisStore.Redis(), log)
	if err := qw.Init(ctx); err != nil {
		log.Fatal("init queue consumer group", zap.Error(err))
	}

	log.Info("worker starting")
	qw.Run(ctx, b.HandleEvent)
	log.Info("worker stopped")
}
