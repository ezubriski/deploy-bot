package bot

import (
	"context"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/yourorg/deploy-bot/internal/audit"
	"github.com/yourorg/deploy-bot/internal/config"
	"github.com/yourorg/deploy-bot/internal/ecr"
	githubPkg "github.com/yourorg/deploy-bot/internal/github"
	"github.com/yourorg/deploy-bot/internal/metrics"
	"github.com/yourorg/deploy-bot/internal/store"
	"github.com/yourorg/deploy-bot/internal/validator"
)

const shutdownDrainTimeout = 30 * time.Second

type Bot struct {
	slack     *slack.Client
	sm        *socketmode.Client
	store     *store.Store
	gh        *githubPkg.Client
	ecrCache  *ecr.Cache
	validator *validator.Validator
	auditLog  *audit.Logger
	metrics   *metrics.Metrics
	cfg       *config.Config
	log       *zap.Logger

	wg        sync.WaitGroup   // tracks in-flight event handlers
	smCancel  context.CancelFunc
}

func New(
	slackClient *slack.Client,
	sm *socketmode.Client,
	store *store.Store,
	gh *githubPkg.Client,
	ecrCache *ecr.Cache,
	validator *validator.Validator,
	auditLog *audit.Logger,
	m *metrics.Metrics,
	cfg *config.Config,
	log *zap.Logger,
) *Bot {
	return &Bot{
		slack:     slackClient,
		sm:        sm,
		store:     store,
		gh:        gh,
		ecrCache:  ecrCache,
		validator: validator,
		auditLog:  auditLog,
		metrics:   m,
		cfg:       cfg,
		log:       log,
	}
}

// Run starts the socket mode event loop in the background and returns
// immediately. Call Shutdown to drain in-flight handlers and stop.
func (b *Bot) Run() {
	smCtx, cancel := context.WithCancel(context.Background())
	b.smCancel = cancel

	go func() {
		for evt := range b.sm.Events {
			switch evt.Type {
			case socketmode.EventTypeSlashCommand:
				b.wg.Add(1)
				go func(e socketmode.Event) {
					defer b.wg.Done()
					b.handleSlashCommand(&e, b.sm)
				}(evt)
			case socketmode.EventTypeInteractive:
				b.wg.Add(1)
				go func(e socketmode.Event) {
					defer b.wg.Done()
					b.handleInteraction(&e, b.sm)
				}(evt)
			case socketmode.EventTypeConnecting:
				b.log.Info("connecting to Slack")
			case socketmode.EventTypeConnected:
				b.log.Info("connected to Slack via socket mode")
			case socketmode.EventTypeConnectionError:
				b.log.Error("Slack socket mode connection error")
			}
		}
	}()

	go func() {
		if err := b.sm.RunContext(smCtx); err != nil && smCtx.Err() == nil {
			b.log.Error("socket mode stopped unexpectedly", zap.Error(err))
		}
	}()
}

// Shutdown stops accepting new events and waits up to 30s for all in-flight
// event handlers to complete.
func (b *Bot) Shutdown(ctx context.Context) {
	b.log.Info("bot shutting down, draining in-flight handlers")

	if b.smCancel != nil {
		b.smCancel()
	}

	waitWithTimeout(&b.wg, shutdownDrainTimeout, b.log)
}

// waitWithTimeout blocks until wg reaches zero or timeout elapses.
func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration, log *zap.Logger) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Info("all in-flight handlers completed")
	case <-time.After(timeout):
		log.Warn("drain timeout reached, some handlers may not have completed",
			zap.Duration("timeout", timeout))
	}
}
