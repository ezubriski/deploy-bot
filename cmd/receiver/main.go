package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/approvers"
	"github.com/ezubriski/deploy-bot/internal/bot"
	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/store"
)

const healthAddr = ":8080"

const approverRefreshInterval = 5 * time.Minute

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
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatal("load config", zap.Error(err))
	}

	secrets, err := config.LoadSecrets(ctx, os.Getenv("AWS_SECRET_NAME"))
	if err != nil {
		log.Fatal("load secrets", zap.Error(err))
	}
	if err := secrets.Validate(); err != nil {
		log.Fatal("invalid secrets", zap.Error(err))
	}

	rdb := redis.NewClient(&redis.Options{Addr: secrets.RedisAddr, Password: secrets.RedisToken})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatal("redis ping", zap.Error(err))
	}
	redisStore := store.New(secrets.RedisAddr, secrets.RedisToken)

	slackClient := slack.New(secrets.SlackBotToken,
		slack.OptionAppLevelToken(secrets.SlackAppToken),
	)

	approverCache := approvers.New(secrets.GitHubToken, slackClient, cfg.GitHub.Org, cfg.GitHub.ApproverTeam, log)
	if err := approverCache.Refresh(ctx); err != nil {
		// Fail open: log the error but continue. The cache will retry on the
		// next tick, and the worker still validates approvers authoritatively.
		log.Warn("approver cache initial refresh failed", zap.Error(err))
	}
	approverCache.StartRefresh(ctx, approverRefreshInterval)

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		})
		log.Info("health server listening", zap.String("addr", healthAddr))
		if err := http.ListenAndServe(healthAddr, mux); err != nil {
			log.Error("health server error", zap.Error(err))
		}
	}()

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
			case socketmode.EventTypeSlashCommand:
				cmd, ok := evt.Data.(slack.SlashCommand)
				if ok && !cfg.Slack.IsChannelAllowed(cmd.ChannelID) {
					sm.Ack(*evt.Request, map[string]interface{}{
						"response_type": "ephemeral",
						"text":          "The `/deploy` command is not available in this channel.",
					})
					continue
				}
				enqueueAndAck(ctx, sm, rdb, evt, log)

			case socketmode.EventTypeInteractive:
				callback, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					sm.Ack(*evt.Request)
					continue
				}
				if callback.Type == slack.InteractionTypeViewSubmission &&
					callback.View.CallbackID == bot.ModalCallbackDeploy {
					validateAndDispatch(ctx, sm, rdb, redisStore, cfg, approverCache, evt, callback, log)
				} else {
					enqueueAndAck(ctx, sm, rdb, evt, log)
				}

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

// enqueueAndAck enqueues the event and ACKs Slack. If enqueue fails the event
// is not ACKed, allowing Slack to retry delivery.
func enqueueAndAck(ctx context.Context, sm *socketmode.Client, rdb *redis.Client, evt socketmode.Event, log *zap.Logger) {
	if err := queue.Enqueue(ctx, rdb, evt); err != nil {
		log.Error("enqueue event", zap.String("type", string(evt.Type)), zap.Error(err))
		return
	}
	sm.Ack(*evt.Request)
}

// validateAndDispatch runs inline validation for deploy modal submissions.
// On failure it ACKs with inline errors (modal stays open). On success it
// enqueues and ACKs normally (modal closes).
func validateAndDispatch(
	ctx context.Context,
	sm *socketmode.Client,
	rdb *redis.Client,
	s *store.Store,
	cfg *config.Config,
	approverCache *approvers.Cache,
	evt socketmode.Event,
	callback slack.InteractionCallback,
	log *zap.Logger,
) {
	values := callback.View.State.Values
	appVal := values[bot.BlockApp][bot.ActionApp].SelectedOption.Value
	approverID := values[bot.BlockApprover][bot.ActionApprover].SelectedUser
	manualTag := values[bot.BlockTagManual][bot.ActionTagManual].Value

	errs := make(map[string]string)

	// Check per-app deploy lock.
	if appVal != "" {
		locked, err := s.IsLocked(ctx, appVal)
		if err != nil {
			log.Error("receiver: check deploy lock", zap.String("app", appVal), zap.Error(err))
		} else if locked {
			errs[bot.BlockApp] = fmt.Sprintf("A deployment of *%s* is already in progress.", appVal)
		}
	}

	// Check approver team membership via cache.
	if approverID != "" && !approverCache.IsApprover(approverID) {
		errs[bot.BlockApprover] = "Selected approver is not a member of the approver team."
	}

	// Validate manual tag override against the app's tag pattern.
	if manualTag != "" && appVal != "" {
		appCfg, ok := cfg.AppByName(appVal)
		if ok && !appCfg.CompiledTagPattern().MatchString(manualTag) {
			errs[bot.BlockTagManual] = fmt.Sprintf(
				"Tag does not match the required pattern for %s. Use /deploy tags %s to list valid tags.",
				appVal, appVal,
			)
		}
	}

	if len(errs) > 0 {
		sm.Ack(*evt.Request, map[string]interface{}{
			"response_action": "errors",
			"errors":          errs,
		})
		return
	}

	enqueueAndAck(ctx, sm, rdb, evt, log)
}
