package sweeper

import (
	"context"
	"fmt"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/store"
)



type Sweeper struct {
	store   *store.Store
	gh      *github.Client
	slack   *slack.Client
	audit   *audit.Logger
	metrics *metrics.Metrics
	cfg     *config.Holder
	log     *zap.Logger
}

func New(
	store *store.Store,
	gh *github.Client,
	slackClient *slack.Client,
	auditLog *audit.Logger,
	m *metrics.Metrics,
	cfg *config.Holder,
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
			if err := s.gh.MergePR(ctx, d.PRNumber, s.cfg.Load().Deployment.MergeMethod); err != nil {
				s.log.Error("recover merge failed", zap.Int("pr", d.PRNumber), zap.Error(err))
				continue
			}
			_ = s.store.Delete(ctx, d.PRNumber)
			s.log.Info("recovered stuck deploy", zap.Int("pr", d.PRNumber))
		}
	}
}

// RunOnce performs a single sweep pass: expires stale deploys, notifies
// requesters/approvers, and refreshes the pending gauge.
func (s *Sweeper) RunOnce(ctx context.Context) {
	expired, err := s.store.GetExpired(ctx)
	if err != nil {
		s.log.Error("sweeper: get expired", zap.Error(err))
		return
	}

	staleDuration, err := s.cfg.Load().StaleDuration()
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
		_ = s.store.PushHistory(ctx, store.HistoryEntry{
			EventType:   audit.EventExpired,
			App:         d.App,
			Tag:         d.Tag,
			PRNumber:    d.PRNumber,
			PRURL:       d.PRURL,
			RequesterID: d.RequesterID,
			CompletedAt: time.Now(),
		})
		_ = s.store.ReleaseLock(ctx, d.App)
		_ = s.store.Delete(ctx, d.PRNumber)
	}

	// Refresh the pending gauge after each sweep pass.
	remaining, err := s.store.GetAll(ctx)
	if err == nil {
		s.metrics.SetPendingDeploys(len(remaining))
	}
}
