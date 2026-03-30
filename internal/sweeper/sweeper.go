package sweeper

import (
	"context"
	"fmt"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"

	"github.com/yourorg/deploy-bot/internal/audit"
	"github.com/yourorg/deploy-bot/internal/config"
	"github.com/yourorg/deploy-bot/internal/github"
	"github.com/yourorg/deploy-bot/internal/metrics"
	"github.com/yourorg/deploy-bot/internal/store"
)

const sweepInterval = 5 * time.Minute

type Sweeper struct {
	store   *store.Store
	gh      *github.Client
	slack   *slack.Client
	audit   *audit.Logger
	metrics *metrics.Metrics
	cfg     *config.Config
	log     *zap.Logger
}

func New(
	store *store.Store,
	gh *github.Client,
	slackClient *slack.Client,
	auditLog *audit.Logger,
	m *metrics.Metrics,
	cfg *config.Config,
	log *zap.Logger,
) *Sweeper {
	return &Sweeper{
		store:   store,
		gh:      gh,
		slack:   slackClient,
		audit:   auditLog,
		metrics: m,
		cfg:     cfg,
		log:     log,
	}
}

// RecoverStuck handles any deployments left in "merging" state on leader startup.
func (s *Sweeper) RecoverStuck(ctx context.Context) {
	deploys, err := s.store.GetAll(ctx)
	if err != nil {
		s.log.Error("sweeper: get all deploys", zap.Error(err))
		return
	}
	for _, d := range deploys {
		if d.State == store.StateMerging {
			s.log.Warn("recovering stuck deploy", zap.Int("pr", d.PRNumber), zap.String("app", d.App))
			if err := s.gh.MergePR(ctx, d.PRNumber, s.cfg.Deployment.MergeMethod); err != nil {
				s.log.Error("recover merge failed", zap.Int("pr", d.PRNumber), zap.Error(err))
				continue
			}
			_ = s.store.Delete(ctx, d.PRNumber)
			s.log.Info("recovered stuck deploy", zap.Int("pr", d.PRNumber))
		}
	}
}

// Start runs the sweeper loop every 5 minutes.
func (s *Sweeper) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sweep(ctx)
			}
		}
	}()
}

func (s *Sweeper) sweep(ctx context.Context) {
	expired, err := s.store.GetExpired(ctx)
	if err != nil {
		s.log.Error("sweeper: get expired", zap.Error(err))
		return
	}

	staleDuration, err := s.cfg.StaleDuration()
	if err != nil {
		staleDuration = 2 * time.Hour
	}
	staleDurationStr := fmt.Sprintf("%v", staleDuration)

	for _, d := range expired {
		s.log.Info("expiring deployment", zap.Int("pr", d.PRNumber), zap.String("app", d.App))

		if err := s.gh.CommentExpired(ctx, d.PRNumber, staleDurationStr); err != nil {
			s.log.Error("comment expired", zap.Error(err))
		}
		if err := s.gh.ClosePR(ctx, d.PRNumber); err != nil {
			s.log.Error("close expired PR", zap.Error(err))
		}

		// DM requester
		_, _, err := s.slack.PostMessageContext(ctx, d.RequesterID,
			slack.MsgOptionText(fmt.Sprintf(
				"Your deployment of *%s* `%s` (PR #%d) expired after %s with no approval.",
				d.App, d.Tag, d.PRNumber, staleDurationStr,
			), false),
		)
		if err != nil {
			s.log.Error("DM requester expired", zap.Error(err))
		}

		// DM approver
		_, _, err = s.slack.PostMessageContext(ctx, d.ApproverID,
			slack.MsgOptionText(fmt.Sprintf(
				"The deployment request for *%s* `%s` (PR #%d) expired after %s.",
				d.App, d.Tag, d.PRNumber, staleDurationStr,
			), false),
		)
		if err != nil {
			s.log.Error("DM approver expired", zap.Error(err))
		}

		_ = s.audit.Log(ctx, audit.AuditEvent{
			EventType: audit.EventExpired,
			App:       d.App,
			Tag:       d.Tag,
			PRNumber:  d.PRNumber,
			PRURL:     d.PRURL,
			Requester: d.Requester,
		})

		s.metrics.RecordDeploy(d.App, audit.EventExpired)
		_ = s.store.Delete(ctx, d.PRNumber)
	}

	// Refresh the pending gauge after each sweep pass.
	remaining, err := s.store.GetAll(ctx)
	if err == nil {
		s.metrics.SetPendingDeploys(len(remaining))
	}
}
