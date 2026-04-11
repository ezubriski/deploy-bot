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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/approvers"
	"github.com/ezubriski/deploy-bot/internal/bot"
	"github.com/ezubriski/deploy-bot/internal/buffer"
	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/ecr"
	"github.com/ezubriski/deploy-bot/internal/ecrpoller"
	"github.com/ezubriski/deploy-bot/internal/health"
	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/observability"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/reposcanner"
	"github.com/ezubriski/deploy-bot/internal/slackclient"
	"github.com/ezubriski/deploy-bot/internal/store"
)

const healthAddr = ":8080"

const approverRefreshInterval = 5 * time.Minute

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "/etc/deploy-bot/config.json"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	level, err := config.ResolvedLogLevel(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve log level: %v\n", err)
		os.Exit(1)
	}
	format, err := config.ResolvedLogFormat(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve log format: %v\n", err)
		os.Exit(1)
	}
	log, err := config.NewLogger(level, format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync()
	log.Info("logger initialized", zap.Stringer("log_level", level))

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

	obsProvider, err := observability.Setup("deploy-bot-receiver", prometheus.DefaultRegisterer)
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

	hh := &health.Handler{}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", hh.Liveness)

	redisStore, err := store.NewFromSecrets(ctx, secrets)
	if err != nil {
		log.Fatal("init redis store", zap.Error(err))
	}
	if err := observability.InstrumentRedis(redisStore.Redis()); err != nil {
		log.Warn("instrument redis", zap.Error(err))
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
		slack.OptionHTTPClient(observability.HTTPClient(nil)),
	)

	evtBuffer := buffer.New(cfg.Slack.BufferSize, rdb, queue.StreamKeyUser, log)
	go evtBuffer.Run(ctx)

	ghTeams, ghUsers, slackGroups, slackEmails, authErr := config.ParseAuthValues(cfg.Authorization)
	if authErr != nil {
		log.Fatal("parse authorization config", zap.Error(authErr))
	}
	parsedAuth := config.ParsedAuthorization{
		GitHubTeams:     ghTeams,
		GitHubUsers:     ghUsers,
		SlackUserGroups: slackGroups,
		SlackEmails:     slackEmails,
	}
	var memberHTTP *http.Client
	if len(parsedAuth.GitHubTeams) > 0 || len(parsedAuth.GitHubUsers) > 0 {
		memberHTTP, err = secrets.MemberCacheHTTPClient()
		if err != nil {
			log.Fatal("member cache github client", zap.Error(err))
		}
	}
	memberCache := approvers.New(memberHTTP, slackClient, rdb, cfg.GitHub.Org, parsedAuth, cfg.IdentityOverrides, log)
	if err := memberCache.Refresh(ctx); err != nil {
		// Fail open: log the error but continue. The cache will retry on the
		// next tick, and the worker still validates members authoritatively.
		log.Warn("member cache initial refresh failed", zap.Error(err))
	}
	memberCache.StartRefresh(ctx, approverRefreshInterval)

	// ECR cache used for inline manual-tag validation on modal submit.
	// Populate on startup so the in-memory cache is warm from Redis (written
	// by the worker) — this avoids a DescribeImages call on the common path.
	// We skip StartRefresh: the worker already refreshes every 5 minutes and
	// writes to Redis, and cache misses here fall back to a direct ECR
	// DescribeImages via ValidateTag, which is cheap for a single tag.
	m := metrics.NewDefault()
	ecrCache, err := ecr.NewCache(ctx, cfg, rdb, m, log)
	if err != nil {
		log.Fatal("init ecr cache", zap.Error(err))
	}
	ecrCache.Populate(ctx)

	// Start ECR poller and/or webhook if configured. Both share the same
	// buffer and Redis stream for ECR events.
	if cfg.ECRAutoDeploy.Enabled {
		ecrBuf := buffer.New(buffer.DefaultSize, rdb, queue.StreamKeyECR, log)
		go ecrBuf.Run(ctx)
		holder := config.NewHolder(cfg, configPath)

		if cfg.ECRAutoDeploy.SQSQueueURL != "" {
			poller, err := ecrpoller.New(ctx, rdb, ecrBuf, redisStore, holder, log)
			if err != nil {
				log.Fatal("init ecr poller", zap.Error(err))
			}
			go poller.Run(ctx)
		}

		if cfg.ECRAutoDeploy.WebhookEnabled {
			if len(secrets.ECRWebhookAPIKey) < 32 {
				log.Fatal("ecr_webhook_api_key must be at least 32 characters when webhook is enabled")
			}
			webhookPoller := ecrpoller.NewWithoutSQS(rdb, ecrBuf, redisStore, holder, log)
			mux.Handle("/v1/webhooks/ecr", ecrpoller.NewWebhookHandler(webhookPoller, secrets.ECRWebhookAPIKey, m, log))
			log.Info("ecr webhook endpoint registered", zap.String("path", "/v1/webhooks/ecr"))
		}
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

	go func() {
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
					if err := sm.Ack(*evt.Request, map[string]interface{}{
						"response_type": "ephemeral",
						"text":          fmt.Sprintf("The `%s` command is not available in this channel.", cmd.Command),
					}); err != nil {
						log.Error("slack: ack channel-disallowed", zap.Error(err))
					}
					continue
				}
				enqueueAndAck(ctx, sm, rdb, evtBuffer, evt, log)

			case socketmode.EventTypeInteractive:
				callback, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					if err := sm.Ack(*evt.Request); err != nil {
						log.Error("slack: ack", zap.Error(err))
					}
					continue
				}
				if callback.Type == slack.InteractionTypeViewSubmission &&
					callback.View.CallbackID == bot.ModalCallbackDeploy {
					validateAndDispatch(ctx, sm, rdb, cfg, memberCache, ecrCache, evtBuffer, evt, callback, log)
				} else {
					enqueueAndAck(ctx, sm, rdb, evtBuffer, evt, log)
				}

			case socketmode.EventTypeEventsAPI:
				eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					if err := sm.Ack(*evt.Request); err != nil {
						log.Error("slack: ack", zap.Error(err))
					}
					continue
				}
				if err := sm.Ack(*evt.Request); err != nil {
					log.Error("slack: ack", zap.Error(err))
				}
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
	if err := sm.Ack(*evt.Request); err != nil {
		log.Error("slack: ack", zap.Error(err))
	}
}

// validateAndDispatch runs inline validation for deploy modal submissions.
// On failure it ACKs with inline errors (modal stays open). On success it
// enqueues and ACKs normally (modal closes).
func validateAndDispatch(
	ctx context.Context,
	sm *socketmode.Client,
	rdb *redis.Client,
	cfg *config.Config,
	memberCache *approvers.Cache,
	ecrCache *ecr.Cache,
	buf *buffer.Buffer,
	evt socketmode.Event,
	callback slack.InteractionCallback,
	log *zap.Logger,
) {
	vals := bot.ModalValues(callback.View.State.Values)
	appName := vals.SelectedOption(bot.BlockAppName, bot.ActionAppName)
	env := vals.SelectedOption(bot.BlockEnv, bot.ActionEnv)
	appVal := appName + "-" + env // reconstruct FullName
	approverID := vals.SelectedUser(bot.BlockApprover, bot.ActionApprover)
	dropdownTag := vals.SelectedOption(bot.BlockTag, bot.ActionTag)
	manualTag := vals.Text(bot.BlockTagManual, bot.ActionTagManual)

	errs := make(map[string]string)

	// Fast, in-memory checks only. Lock checks are deferred to the worker
	// so the modal responds immediately even under heavy load.

	if appName == "" {
		errs[bot.BlockAppName] = "Application is required."
	}
	if env == "" {
		errs[bot.BlockEnv] = "Environment is required."
	}

	// Check team membership via cache (in-memory).
	if approverID != "" && !memberCache.IsMember(approverID) {
		errs[bot.BlockApprover] = "Selected approver is not a member of the authorized team."
	}

	// Validate manual tag override against the app's tag pattern (in-memory).
	if manualTag != "" && appName != "" && env != "" {
		appCfg, ok := cfg.AppByName(appVal)
		if ok && !appCfg.CompiledTagPattern().MatchString(manualTag) {
			errs[bot.BlockTagManual] = fmt.Sprintf(
				"Tag does not match the required pattern for %s. Use /deploy tags %s to list valid tags.",
				appVal, appVal,
			)
		}
	}

	// Require either a selected tag or a manual tag. Without this the worker
	// would fail later with a channel-side notice and the modal would close
	// silently — confusing UX.
	if _, hasTagErr := errs[bot.BlockTagManual]; !hasTagErr && dropdownTag == "" && manualTag == "" && appName != "" && env != "" {
		errs[bot.BlockTagManual] = "Enter a tag or pick one from the dropdown above."
	}

	// ECR existence check for the manual tag — authoritative (cache hit or
	// direct DescribeImages fallback). Fail open on errors (API blips,
	// unknown app from hot-reload race) so the worker gets a chance to
	// validate again. Skip if we already queued a pattern error.
	if _, hasTagErr := errs[bot.BlockTagManual]; !hasTagErr && manualTag != "" && appName != "" && env != "" {
		exists, vErr := ecrCache.ValidateTag(ctx, appVal, manualTag)
		if vErr != nil {
			log.Warn("receiver: ecr validate failed, passing through", zap.String("app", appVal), zap.String("tag", manualTag), zap.Error(vErr))
		} else if !exists {
			errs[bot.BlockTagManual] = fmt.Sprintf(
				"Tag %q was not found in ECR for %s. Use /deploy tags %s to list valid tags.",
				manualTag, appVal, appVal,
			)
		}
	}

	if len(errs) > 0 {
		if err := sm.Ack(*evt.Request, map[string]interface{}{
			"response_action": "errors",
			"errors":          errs,
		}); err != nil {
			log.Error("slack: ack modal validation errors", zap.Error(err))
		}
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
