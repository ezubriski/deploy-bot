package sweeper

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/store"
)

// recoveryCandidate holds a parsed open PR that is missing from Redis.
type recoveryCandidate struct {
	number int
	prURL  string
	meta   *github.PRMeta
}

// ReconcileFromGitHub scans open PRs carrying the pending label and closes any
// that are missing from Redis — which happens after a cache flush. Each
// requester is notified with the exact command to reproduce their request.
//
// PRs are grouped by app so requesters are aware of concurrent requests for the
// same app when deciding whether to re-request.
func (s *Sweeper) ReconcileFromGitHub(ctx context.Context) {
	cfg := s.cfg.Load()

	issues, err := s.gh.ListOpenPRsWithLabel(ctx, cfg.PendingLabel())
	if err != nil {
		s.log.Error("reconcile: list labeled PRs", zap.Error(err))
		return
	}

	// Filter to PRs not already tracked in Redis and group by app.
	byApp := make(map[string][]recoveryCandidate)
	for _, issue := range issues {
		prNumber := issue.GetNumber()

		existing, err := s.store.Get(ctx, prNumber)
		if err != nil {
			s.log.Error("reconcile: check store", zap.Int("pr", prNumber), zap.Error(err))
			continue
		}
		if existing != nil {
			continue
		}

		meta, ok := github.ParsePRMeta(issue.GetBody())
		if !ok {
			s.log.Warn("reconcile: PR has no metadata, skipping", zap.Int("pr", prNumber))
			continue
		}

		byApp[meta.App] = append(byApp[meta.App], recoveryCandidate{
			number: prNumber,
			prURL:  issue.GetHTMLURL(),
			meta:   meta,
		})
	}

	closed := 0

	for _, candidates := range byApp {
		// Sort oldest first so the context message lists them chronologically.
		slices.SortFunc(candidates, func(a, b recoveryCandidate) int {
			return cmp.Compare(a.number, b.number)
		})

		for i, c := range candidates {
			_ = s.gh.ClosePR(ctx, c.number)
			_ = s.gh.RemoveLabel(ctx, c.number, cfg.PendingLabel())
			_ = s.store.ReleaseLock(ctx, c.meta.App)

			others := make([]recoveryCandidate, 0, len(candidates)-1)
			others = append(others, candidates[:i]...)
			others = append(others, candidates[i+1:]...)

			s.notifyRecoveryClose(ctx, c, others)
			closed++
			s.log.Info("reconcile: closed and notified",
				zap.Int("pr", c.number),
				zap.String("app", c.meta.App),
			)
		}
	}

	if closed > 0 {
		s.log.Info("reconcile: complete", zap.Int("closed", closed))
	}
}

// notifyRecoveryClose DMs the requester that their PR was closed after a
// restart and gives them the exact command to re-request. If other concurrent
// requests for the same app were found, they are listed for context.
func (s *Sweeper) notifyRecoveryClose(ctx context.Context, c recoveryCandidate, others []recoveryCandidate) {
	var sb strings.Builder
	fmt.Fprintf(&sb,
		":warning: Your deployment of *%s* `%s` (<%s|PR #%d>) was closed after a system restart.\n\nTo re-request: `/deploy %s` and select tag `%s`.",
		c.meta.App, c.meta.Tag, c.prURL, c.number, c.meta.App, c.meta.Tag,
	)

	if len(others) > 0 {
		sb.WriteString("\n\n*Note:* the following concurrent deployment requests for this app were also found and closed:")
		for _, o := range others {
			fmt.Fprintf(&sb, "\n• <%s|PR #%d> `%s` by <@%s>", o.prURL, o.number, o.meta.Tag, o.meta.RequesterSlackID)
		}
	}

	_, _, _ = s.slack.PostMessageContext(ctx, c.meta.RequesterSlackID, slack.MsgOptionText(sb.String(), false))
}



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
			_ = s.gh.RemoveLabel(ctx, d.PRNumber, s.cfg.Load().PendingLabel())
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
		_ = s.gh.RemoveLabel(ctx, d.PRNumber, s.cfg.Load().PendingLabel())

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
