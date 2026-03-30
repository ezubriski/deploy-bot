package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/yourorg/deploy-bot/internal/audit"
	"github.com/yourorg/deploy-bot/internal/bot"
	"github.com/yourorg/deploy-bot/internal/config"
	"github.com/yourorg/deploy-bot/internal/ecr"
	"github.com/yourorg/deploy-bot/internal/election"
	githubPkg "github.com/yourorg/deploy-bot/internal/github"
	"github.com/yourorg/deploy-bot/internal/metrics"
	"github.com/yourorg/deploy-bot/internal/store"
	"github.com/yourorg/deploy-bot/internal/sweeper"
	"github.com/yourorg/deploy-bot/internal/validator"
)

const metricsAddr = ":9090"

func main() {
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync()

	ctx := context.Background()

	// Load config
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "/etc/deploy-bot/config.json"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatal("load config", zap.Error(err))
	}

	// Load secrets from AWS Secrets Manager
	secrets, err := loadSecrets(ctx, os.Getenv("AWS_SECRET_NAME"))
	if err != nil {
		log.Fatal("load secrets", zap.Error(err))
	}
	if err := secrets.Validate(); err != nil {
		log.Fatal("invalid secrets", zap.Error(err))
	}

	// Metrics — start HTTP server on :9090 immediately so it's always scrapeable.
	m := metrics.NewDefault()
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		log.Info("metrics server listening", zap.String("addr", metricsAddr))
		if err := http.ListenAndServe(metricsAddr, mux); err != nil {
			log.Error("metrics server error", zap.Error(err))
		}
	}()

	// Redis store
	redisStore := store.New(secrets.RedisAddr)
	if err := redisStore.Ping(ctx); err != nil {
		log.Fatal("redis ping", zap.Error(err))
	}

	// GitHub client
	ghClient := githubPkg.NewClient(secrets.GitHubToken, cfg.GitHub.Org, cfg.GitHub.Repo)

	// Slack client
	slackClient := slack.New(secrets.SlackBotToken,
		slack.OptionAppLevelToken(secrets.SlackAppToken),
	)
	sm := socketmode.New(slackClient,
		socketmode.OptionDebug(false),
	)

	// ECR cache
	ecrCache, err := ecr.NewCache(ctx, cfg, m, log)
	if err != nil {
		log.Fatal("init ecr cache", zap.Error(err))
	}

	// Audit logger
	auditLog, err := audit.NewLogger(ctx, cfg, log)
	if err != nil {
		log.Fatal("init audit logger", zap.Error(err))
	}

	// Validator
	val := validator.New(secrets.GitHubToken, slackClient, cfg, log)

	// Bot
	b := bot.New(slackClient, sm, redisStore, ghClient, ecrCache, val, auditLog, m, cfg, log)

	// Sweeper
	sw := sweeper.New(redisStore, ghClient, slackClient, auditLog, m, cfg, log)

	// Kubernetes identity for leader election
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		podName = "deploy-bot-local"
	}
	podNamespace := os.Getenv("POD_NAMESPACE")
	if podNamespace == "" {
		podNamespace = "default"
	}

	callbacks := election.Callbacks{
		OnStartedLeading: func(leaderCtx context.Context) {
			log.Info("became leader", zap.String("pod", podName))

			// Populate ECR cache, fail open
			ecrCache.Populate(leaderCtx)
			ecrCache.StartRefresh(leaderCtx)

			// Recover stuck deploys
			sw.RecoverStuck(leaderCtx)
			sw.Start(leaderCtx)

			// Run the bot (blocks until context cancelled)
			b.Run()
		},
	}

	log.Info("starting leader election", zap.String("pod", podName), zap.String("namespace", podNamespace))
	if err := election.Run(ctx, podName, podNamespace, callbacks, log); err != nil {
		log.Fatal("leader election", zap.Error(err))
	}
}

func loadSecrets(ctx context.Context, secretName string) (*config.Secrets, error) {
	if secretName == "" {
		return nil, fmt.Errorf("AWS_SECRET_NAME not set")
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := secretsmanager.NewFromConfig(cfg)
	out, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretName),
	})
	if err != nil {
		return nil, fmt.Errorf("get secret: %w", err)
	}

	var secrets config.Secrets
	if err := json.Unmarshal([]byte(aws.ToString(out.SecretString)), &secrets); err != nil {
		return nil, fmt.Errorf("parse secrets: %w", err)
	}
	return &secrets, nil
}
