package bot

import (
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/yourorg/deploy-bot/internal/audit"
	"github.com/yourorg/deploy-bot/internal/config"
	"github.com/yourorg/deploy-bot/internal/ecr"
	githubPkg "github.com/yourorg/deploy-bot/internal/github"
	"github.com/yourorg/deploy-bot/internal/store"
	"github.com/yourorg/deploy-bot/internal/validator"
)

type Bot struct {
	slack     *slack.Client
	sm        *socketmode.Client
	store     *store.Store
	gh        *githubPkg.Client
	ecrCache  *ecr.Cache
	validator *validator.Validator
	auditLog  *audit.Logger
	cfg       *config.Config
	log       *zap.Logger
}

func New(
	slackClient *slack.Client,
	sm *socketmode.Client,
	store *store.Store,
	gh *githubPkg.Client,
	ecrCache *ecr.Cache,
	validator *validator.Validator,
	auditLog *audit.Logger,
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
		cfg:       cfg,
		log:       log,
	}
}

// Run starts the socket mode event loop. Blocks until the client disconnects.
func (b *Bot) Run() {
	go func() {
		for evt := range b.sm.Events {
			switch evt.Type {
			case socketmode.EventTypeSlashCommand:
				b.handleSlashCommand(&evt, b.sm)
			case socketmode.EventTypeInteractive:
				b.handleInteraction(&evt, b.sm)
			case socketmode.EventTypeConnecting:
				b.log.Info("connecting to Slack")
			case socketmode.EventTypeConnected:
				b.log.Info("connected to Slack via socket mode")
			case socketmode.EventTypeConnectionError:
				b.log.Error("Slack socket mode connection error")
			}
		}
	}()

	if err := b.sm.Run(); err != nil {
		b.log.Fatal("socket mode run failed", zap.Error(err))
	}
}
