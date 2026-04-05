package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/approvers"
	"github.com/ezubriski/deploy-bot/internal/bot"
	"github.com/ezubriski/deploy-bot/internal/buffer"
	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/ecrpoller"
	"github.com/ezubriski/deploy-bot/internal/health"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/reposcanner"
	"github.com/ezubriski/deploy-bot/internal/slackclient"
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
	if secrets.SlackAppToken == "" {
		log.Fatal("slack_app_token is required for the receiver (Socket Mode)")
	}

	hh := &health.Handler{}
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", hh.Liveness)
		log.Info("health server listening", zap.String("addr", healthAddr))
		if err := http.ListenAndServe(healthAddr, mux); err != nil {
			log.Error("health server error", zap.Error(err))
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
	rdb := redisStore.Redis()

	slackClient := slack.New(secrets.SlackBotToken,
		slack.OptionAppLevelToken(secrets.SlackAppToken),
	)

	evtBuffer := buffer.New(cfg.Slack.BufferSize, rdb, queue.StreamKeyUser, log)
	go evtBuffer.Run(ctx)

	ghHTTP, err := secrets.GitHubHTTPClient()
	if err != nil {
		log.Fatal("github client", zap.Error(err))
	}
	approverCache := approvers.New(ghHTTP, slackClient, cfg.GitHub.Org, cfg.GitHub.ApproverTeam, log)
	if err := approverCache.Refresh(ctx); err != nil {
		// Fail open: log the error but continue. The cache will retry on the
		// next tick, and the worker still validates approvers authoritatively.
		log.Warn("approver cache initial refresh failed", zap.Error(err))
	}
	approverCache.StartRefresh(ctx, approverRefreshInterval)

	// Start ECR poller if configured. It enqueues to the same Redis stream
	// as Slack events, using its own buffer for Redis backpressure.
	if cfg.ECREvents.SQSQueueURL != "" {
		ecrBuf := buffer.New(buffer.DefaultSize, rdb, queue.StreamKeyECR, log)
		go ecrBuf.Run(ctx)
		poller, err := ecrpoller.New(ctx, rdb, ecrBuf, redisStore, config.NewHolder(cfg, configPath), log)
		if err != nil {
			log.Fatal("init ecr poller", zap.Error(err))
		}
		go poller.Run(ctx)
	}

	// Start repo scanner if configured.
	if cfg.RepoDiscovery.Enabled {
		slackMaxRetries, slackRetryWait := cfg.Slack.RateLimitConfig()
		scannerSlack := slackclient.New(slackClient, slackMaxRetries, slackRetryWait, log)
		var cmWriter reposcanner.ConfigMapWriter
		if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
			w, err := reposcanner.NewK8sConfigMapWriter()
			if err != nil {
				log.Fatal("init configmap writer", zap.Error(err))
			}
			cmWriter = w
		} else {
			log.Warn("reposcanner: not running in Kubernetes, ConfigMap writes disabled")
		}
		cfgHolder := config.NewHolder(cfg, configPath)
		scannerHTTP, scannerErr := secrets.ScannerHTTPClient()
		if scannerErr != nil {
			log.Fatal("scanner github client", zap.Error(scannerErr))
		}
		scanner := reposcanner.NewScanner(scannerHTTP, cfg.GitHub.Org, scannerSlack, cmWriter, cfgHolder, log)
		go scanner.Run(ctx)
	}

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
						"text":          fmt.Sprintf("The `%s` command is not available in this channel.", cmd.Command),
					})
					continue
				}
				enqueueAndAck(ctx, sm, rdb, evtBuffer, evt, log)

			case socketmode.EventTypeInteractive:
				callback, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					sm.Ack(*evt.Request)
					continue
				}
				if callback.Type == slack.InteractionTypeViewSubmission &&
					callback.View.CallbackID == bot.ModalCallbackDeploy {
					validateAndDispatch(ctx, sm, rdb, cfg, approverCache, evtBuffer, evt, callback, log)
				} else {
					enqueueAndAck(ctx, sm, rdb, evtBuffer, evt, log)
				}

			case socketmode.EventTypeEventsAPI:
				eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					sm.Ack(*evt.Request)
					continue
				}
				sm.Ack(*evt.Request)
				handleEventsAPI(ctx, rdb, evtBuffer, eventsAPIEvent, log)

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
// is placed in the buffer for retry — Slack is not ACKed and will retry
// delivery in parallel, providing a second path if the receiver restarts.
func enqueueAndAck(ctx context.Context, sm *socketmode.Client, rdb *redis.Client, buf *buffer.Buffer, evt socketmode.Event, log *zap.Logger) {
	if err := queue.Enqueue(ctx, rdb, evt); err != nil {
		log.Error("enqueue failed, buffering for retry", zap.String("type", string(evt.Type)), zap.Error(err))
		buf.Add(evt)
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
	cfg *config.Config,
	approverCache *approvers.Cache,
	buf *buffer.Buffer,
	evt socketmode.Event,
	callback slack.InteractionCallback,
	log *zap.Logger,
) {
	vals := bot.ModalValues(callback.View.State.Values)
	appVal := vals.SelectedOption(bot.BlockApp, bot.ActionApp)
	approverID := vals.SelectedUser(bot.BlockApprover, bot.ActionApprover)
	manualTag := vals.Text(bot.BlockTagManual, bot.ActionTagManual)

	errs := make(map[string]string)

	// Fast, in-memory checks only. Lock checks are deferred to the worker
	// so the modal responds immediately even under heavy load.

	// Check approver team membership via cache (in-memory).
	if approverID != "" && !approverCache.IsApprover(approverID) {
		errs[bot.BlockApprover] = "Selected approver is not a member of the approver team."
	}

	// Validate manual tag override against the app's tag pattern (in-memory).
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

	enqueueAndAck(ctx, sm, rdb, buf, evt, log)
}

// handleEventsAPI processes Events API events received via socket mode.
// Currently only app_mention is handled.
func handleEventsAPI(ctx context.Context, rdb *redis.Client, buf *buffer.Buffer, apiEvent slackevents.EventsAPIEvent, log *zap.Logger) {
	switch apiEvent.InnerEvent.Type {
	case string(slackevents.AppMention):
		mention, ok := apiEvent.InnerEvent.Data.(*slackevents.AppMentionEvent)
		if !ok {
			return
		}
		// Strip the <@BOTID> prefix from the text.
		text := stripMentionPrefix(mention.Text)

		evt := queue.NewAppMentionEvent(queue.AppMentionEvent{
			UserID:   mention.User,
			Channel:  mention.Channel,
			Text:     text,
			ThreadTS: mention.ThreadTimeStamp,
		})
		if err := queue.Enqueue(ctx, rdb, evt); err != nil {
			log.Error("enqueue mention failed, buffering", zap.Error(err))
			buf.Add(evt)
		}

	default:
		log.Debug("receiver: unhandled events API type", zap.String("type", apiEvent.InnerEvent.Type))
	}
}

// stripMentionPrefix removes the leading <@USERID> mention from the text,
// returning the remainder trimmed of whitespace.
func stripMentionPrefix(text string) string {
	// Slack formats mentions as "<@U12345> command args"
	if idx := strings.Index(text, ">"); idx >= 0 && strings.HasPrefix(text, "<@") {
		return strings.TrimSpace(text[idx+1:])
	}
	return strings.TrimSpace(text)
}
