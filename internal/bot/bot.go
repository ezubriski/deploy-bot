package bot

import (
	"context"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/ecr"
	githubPkg "github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/store"
	"github.com/ezubriski/deploy-bot/internal/validator"
)

type Bot struct {
	slack     *slack.Client
	store     *store.Store
	gh        *githubPkg.Client
	ecrCache  *ecr.Cache
	validator *validator.Validator
	auditLog  *audit.Logger
	metrics   *metrics.Metrics
	cfg       *config.Holder
	log       *zap.Logger
}

func New(
	slackClient *slack.Client,
	store *store.Store,
	gh *githubPkg.Client,
	ecrCache *ecr.Cache,
	validator *validator.Validator,
	auditLog *audit.Logger,
	m *metrics.Metrics,
	cfg *config.Holder,
	log *zap.Logger,
) *Bot {
	return &Bot{
		slack:     slackClient,
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

// HandleEvent routes a decoded socketmode.Event to the appropriate handler.
// It is called synchronously by the queue worker for each event.
func (b *Bot) HandleEvent(ctx context.Context, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeSlashCommand:
		b.handleSlashCommand(ctx, evt)
	case socketmode.EventTypeInteractive:
		b.handleInteraction(ctx, evt)
	default:
		b.log.Warn("bot: unhandled event type", zap.String("type", string(evt.Type)))
	}
}
