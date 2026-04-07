package main

import (
	"context"
	"fmt"
	"net/http"
	pprofhttp "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	"github.com/ezubriski/deploy-bot/internal/slackclient"
	"github.com/ezubriski/deploy-bot/internal/store"
	"github.com/ezubriski/deploy-bot/internal/sweeper"
	"github.com/ezubriski/deploy-bot/internal/validator"
)

const (
	metricsAddr  = ":9090"
	sweepLockTTL = 6 * time.Minute // slightly longer than sweepInterval
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
	// Load primary config first to check if repo discovery is enabled,
	// then reload with discovered apps merged in.
	primaryCfg, err := config.Load(configPath)
	if err != nil {
		log.Fatal("load config", zap.Error(err))
	}
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
		mux.HandleFunc("/debug/pprof/", pprofhttp.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprofhttp.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprofhttp.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprofhttp.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprofhttp.Trace)
		log.Info("metrics/health server listening", zap.String("addr", metricsAddr))
		if err := http.ListenAndServe(metricsAddr, mux); err != nil {
			log.Error("metrics/health server error", zap.Error(err))
		}
	}()

	redisStore, err := store.NewFromSecrets(ctx, secrets)
	if err != nil {
		log.Fatal("init redis store", zap.Error(err))
	}
	log.Info("waiting for redis", zap.String("addr", secrets.RedisAddr), zap.Bool("iam_auth", secrets.RedisIAMAuth))
	if err := redisStore.WaitForRedis(ctx, time.Minute); err != nil {
		log.Fatal("redis not available", zap.Error(err))
	}
	hh.SetHealthy()
	log.Info("redis connected")

	ghHTTP, err := secrets.GitHubHTTPClient(cfgHolder.Load().GitHub.Repo)
	if err != nil {
		log.Fatal("github client", zap.Error(err))
	}
	maxRetries, retryWait := cfgHolder.Load().GitHub.RateLimitConfig()
	ghClient := githubPkg.NewClient(ghHTTP, cfgHolder.Load().GitHub.Org, cfgHolder.Load().GitHub.Repo, log, githubPkg.RetryConfig{MaxRetries: maxRetries, RetryWait: retryWait})

	for _, label := range []string{cfgHolder.Load().DeployLabel(), cfgHolder.Load().PendingLabel()} {
		if err := ghClient.EnsureLabel(ctx, label, githubPkg.LabelColor); err != nil {
			log.Warn("ensure label", zap.String("label", label), zap.Error(err))
		}
	}

	slackOpts := []slack.Option{}
	if secrets.SlackAppToken != "" {
		slackOpts = append(slackOpts, slack.OptionAppLevelToken(secrets.SlackAppToken))
	}
	rawSlack := slack.New(secrets.SlackBotToken, slackOpts...)
	slackMaxRetries, slackRetryWait := cfgHolder.Load().Slack.RateLimitConfig()
	slackClient := slackclient.New(rawSlack, slackMaxRetries, slackRetryWait, log)

	ecrCache, err := ecr.NewCache(ctx, cfgHolder.Load(), redisStore.Redis(), m, log)
	if err != nil {
		log.Fatal("init ecr cache", zap.Error(err))
	}

	auditLog, err := audit.NewLogger(ctx, cfgHolder.Load(), log)
	if err != nil {
		log.Fatal("init audit logger", zap.Error(err))
	}

	valHTTP, err := secrets.ValidatorHTTPClient()
	if err != nil {
		log.Fatal("validator github client", zap.Error(err))
	}
	val := validator.New(valHTTP, rawSlack, redisStore.Redis(), cfgHolder.Load(), log)

	// Log prod auto-deploy guard status at startup.
	logProdAutoDeployGuard(initialCfg, auditLog, log)

	b := bot.New(slackClient, redisStore, ghClient, ecrCache, val, auditLog, m, cfgHolder, log)
	sw := sweeper.New(redisStore, ghClient, slackClient, auditLog, m, cfgHolder, log)

	config.Watch(ctx, cfgHolder, 30*time.Second, func(newCfg *config.Config) {
		if err := ecrCache.AddApps(newCfg.Apps); err != nil {
			log.Warn("ecr cache: failed to register new apps after reload", zap.Error(err))
		}
	}, log)

	// Populate ECR cache; mark ready once done. This blocks so the readiness
	// probe gates traffic until tags are loaded.
	ecrCache.Populate(ctx)
	hh.SetCacheReady()
	ecrCache.StartRefresh(ctx)

	// Run startup recovery tasks concurrently — they don't need to finish
	// before the worker starts processing events.
	go func() {
		sw.RecoverStuck(ctx)
		sw.ReconcileFromGitHub(ctx)
		sw.ReconstructHistory(ctx)
	}()

	// Optional periodic reconciliation (disabled by default).
	if interval := cfgHolder.Load().Deployment.ReconcileInterval; interval != "" {
		reconcileInterval, err := time.ParseDuration(interval)
		if err != nil {
			log.Warn("invalid reconcile_interval, periodic reconciliation disabled", zap.Error(err))
		} else {
			go func() {
				ticker := time.NewTicker(reconcileInterval)
				defer ticker.Stop()
				lockTTL := reconcileInterval + time.Minute
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						acquired, err := redisStore.TryLock(ctx, "reconcile", lockTTL)
						if err != nil {
							log.Error("reconcile lock", zap.Error(err))
							continue
						}
						if acquired {
							sw.ReconcileFromGitHub(ctx)
						}
					}
				}
			}()
		}
	}

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

// logProdAutoDeployGuard logs the status of production auto-deploy at startup.
func logProdAutoDeployGuard(cfg *config.Config, auditLog audit.Logger, log *zap.Logger) {
	var prodAutoDeployApps []string
	for _, app := range cfg.Apps {
		if !app.AutoDeploy {
			continue
		}
		if app.IsProd() && !cfg.Deployment.AllowProdAutoDeploy {
			log.Warn("auto_deploy ignored for prod app (allow_prod_auto_deploy is false)",
				zap.String("app", app.FullName()),
				zap.String("environment", app.Environment),
			)
			continue
		}
		if app.IsProd() {
			prodAutoDeployApps = append(prodAutoDeployApps, app.FullName())
		}
	}
	if len(prodAutoDeployApps) > 0 {
		log.Info("production apps with auto-deploy enabled",
			zap.Strings("apps", prodAutoDeployApps),
		)
		_ = auditLog.Log(context.Background(), audit.AuditEvent{
			EventType: audit.EventStartup,
			Trigger:   audit.TriggerStartup,
			Reason:    fmt.Sprintf("prod auto-deploy apps: %v", prodAutoDeployApps),
		})
	}
}
