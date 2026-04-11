package bot

import (
	"context"

	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/ecr"
	githubpkg "github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/slackclient"
	"github.com/ezubriski/deploy-bot/internal/store"
	"github.com/ezubriski/deploy-bot/internal/validator"
)

// githubClient is the subset of *githubpkg.Client methods used by the bot.
// Defined as an interface to allow test doubles.
type githubClient interface {
	GetDefaultBranch(ctx context.Context) (string, error)
	CreateDeployPR(ctx context.Context, params githubpkg.CreatePRParams) (int, string, error)
	RebaseDeployBranch(ctx context.Context, params githubpkg.CreatePRParams) error
	MergePR(ctx context.Context, prNumber int, mergeMethod string) (string, error)
	ClosePR(ctx context.Context, prNumber int) error
	DeleteBranch(ctx context.Context, branch string) error
	CommentRequested(ctx context.Context, prNumber int, requester, app, tag, reason string) error
	CommentApproved(ctx context.Context, prNumber int, approver string) error
	CommentRejected(ctx context.Context, prNumber int, approver, reason string) error
	CommentExpired(ctx context.Context, prNumber int, staleDuration string) error
	CommentCancelled(ctx context.Context, prNumber int, requester string) error
	CommentNoOp(ctx context.Context, prNumber int, app, tag string) error
	CommentAutoDeployFailed(ctx context.Context, prNumber int, reason error) error
	AddLabels(ctx context.Context, prNumber int, labels []string) error
	RemoveLabel(ctx context.Context, prNumber int, label string) error
}

// deployValidator is the subset of *validator.Validator methods used by the bot.
type deployValidator interface {
	IsMember(ctx context.Context, slackID string) (bool, validator.Identity, error)
	ResolveIdentity(ctx context.Context, slackID string) (validator.Identity, error)
	SlackUserToGitHub(ctx context.Context, slackID string) (string, error)
}

// tagCache is the subset of *ecr.Cache methods used by the bot.
type tagCache interface {
	ValidateTag(ctx context.Context, app, tag string) (bool, error)
	RecentTags(app string) []string
	RecentTagsWithTime(app string) []ecr.TagWithTime
	Tags(app string, n int) []string
}

type Bot struct {
	slack     slackclient.Poster
	store     *store.Store
	gh        githubClient
	ecrCache  tagCache
	validator deployValidator
	auditLog  audit.Logger
	metrics   *metrics.Metrics
	cfg       *config.Holder
	log       *zap.Logger
}

func New(
	slackClient slackclient.Poster,
	store *store.Store,
	gh *githubpkg.Client,
	ecrCache *ecr.Cache,
	validator *validator.Validator,
	auditLog audit.Logger,
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
	case queue.EventTypeECRPush:
		ecrEvt, ok := evt.Data.(queue.ECRPushEvent)
		if !ok {
			b.log.Error("bot: ecr_push event has unexpected data type")
			return
		}
		b.handleECRPush(ctx, ecrEvt)
	case queue.EventTypeAppMention:
		mention, ok := evt.Data.(queue.AppMentionEvent)
		if !ok {
			b.log.Error("bot: app_mention event has unexpected data type")
			return
		}
		b.handleMention(ctx, mention)
	case queue.EventTypeArgoCDNotification:
		argo, ok := evt.Data.(queue.ArgoCDNotificationEvent)
		if !ok {
			b.log.Error("bot: argocd_notification event has unexpected data type")
			return
		}
		b.handleArgoCDNotification(ctx, argo)
	default:
		b.log.Warn("bot: unhandled event type", zap.String("type", string(evt.Type)))
	}
}
