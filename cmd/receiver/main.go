package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/yourorg/deploy-bot/internal/config"
	"github.com/yourorg/deploy-bot/internal/queue"
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

	secrets, err := config.LoadSecrets(ctx, os.Getenv("AWS_SECRET_NAME"))
	if err != nil {
		log.Fatal("load secrets", zap.Error(err))
	}
	if err := secrets.Validate(); err != nil {
		log.Fatal("invalid secrets", zap.Error(err))
	}

	rdb := redis.NewClient(&redis.Options{Addr: secrets.RedisAddr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatal("redis ping", zap.Error(err))
	}

	slackClient := slack.New(secrets.SlackBotToken,
		slack.OptionAppLevelToken(secrets.SlackAppToken),
	)
	sm := socketmode.New(slackClient)

	log.Info("receiver starting")

	go func() {
		if err := sm.RunContext(ctx); err != nil && ctx.Err() == nil {
			log.Error("socket mode stopped", zap.Error(err))
		}
	}()

	for {
		select {
		case <-ctx.Done():
			log.Info("receiver shutting down")
			return
		case evt, ok := <-sm.Events:
			if !ok {
				return
			}
			switch evt.Type {
			case socketmode.EventTypeSlashCommand, socketmode.EventTypeInteractive:
				if err := queue.Enqueue(ctx, rdb, evt); err != nil {
					log.Error("enqueue event", zap.String("type", string(evt.Type)), zap.Error(err))
					// Do not ack — let Slack retry.
					continue
				}
				sm.Ack(*evt.Request)
			case socketmode.EventTypeConnecting:
				log.Info("connecting to Slack")
			case socketmode.EventTypeConnected:
				log.Info("connected to Slack via socket mode")
			case socketmode.EventTypeConnectionError:
				log.Error("Slack connection error")
			}
		}
	}
}
