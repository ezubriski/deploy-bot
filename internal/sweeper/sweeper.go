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

	gh "github.com/google/go-github/v60/github"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/slackclient"
	"github.com/ezubriski/deploy-bot/internal/store"
)

// ReconstructHistory asynchronously populates the deployment history from
// GitHub commit history if the Redis history list is empty. This recovers
// display data after a Redis flush; entries will be missing requester IDs
// and PR links since those are not derivable from commit messages alone.
func (s *Sweeper) ReconstructHistory(ctx context.Context) {
	existing, err := s.store.GetHistory(ctx, 1)
	if err != nil {
		s.log.Error("reconstruct history: check existing", zap.Error(err))
		return
	}
	if len(existing) > 0 {
		return // already populated
	}

	cfg := s.cfg.Load()
	s.log.Info("reconstruct history: history empty, fetching from GitHub")

	var all []github.DeployCommit
	for _, app := range cfg.Apps {
		commits, err := s.gh.ListDeployCommits(ctx, app.KustomizePath, store.HistoryMaxLen)
		if err != nil {
			s.log.Warn("reconstruct history: list commits",
				zap.String("app", app.App), zap.Error(err))
			continue
		}
		all = append(all, commits...)
	}

	if len(all) == 0 {
		s.log.Info("reconstruct history: no deploy commits found")
		return
	}

	// Sort oldest-first: LPUSH prepends, so pushing oldest-first leaves the
	// list in newest-first order after all pushes complete.
	slices.SortFunc(all, func(a, b github.DeployCommit) int {
		return a.CommittedAt.Compare(b.CommittedAt)
	})
	if len(all) > store.HistoryMaxLen {
		all = all[len(all)-store.HistoryMaxLen:]
	}

	pushed := 0
	for _, c := range all {
		prNumber, prURL, err := s.gh.PRForCommit(ctx, c.SHA)
		if err != nil {
			s.log.Warn("reconstruct history: lookup PR for commit",
				zap.String("sha", c.SHA), zap.Error(err))
		}
		entry := store.HistoryEntry{
			EventType:   audit.EventApproved,
			App:         c.App,
			Tag:         c.Tag,
			PRNumber:    prNumber,
			PRURL:       prURL,
			CompletedAt: c.CommittedAt,
		}
		if err := s.store.PushHistory(ctx, entry); err != nil {
			s.log.Error("reconstruct history: push entry", zap.Error(err))
			continue
		}
		pushed++
	}
	s.log.Info("reconstruct history: complete", zap.Int("pushed", pushed))
}

// recoveryCandidate holds a parsed open PR that is missing from Redis.
type recoveryCandidate struct {
	number int
	prURL  string
	meta   *github.PRMeta
}

// ReconcileFromGitHub scans open PRs carrying the deploy-bot label (pending or
// base) and closes any that are missing from Redis — which happens after a
// cache flush or when labels weren't fully applied. Each requester is notified
// with the exact command to reproduce their request.
//
// PRs are grouped by app so requesters are aware of concurrent requests for the
// same app when deciding whether to re-request.
func (s *Sweeper) ReconcileFromGitHub(ctx context.Context) {
	cfg := s.cfg.Load()

	// Search both the pending and base labels to catch orphaned PRs that
	// may have lost their pending label or never received it.
	seen := map[int]struct{}{}
	var allIssues []*gh.Issue
	for _, label := range []string{cfg.PendingLabel(), cfg.DeployLabel()} {
		issues, err := s.gh.ListOpenPRsWithLabel(ctx, label)
		if err != nil {
			s.log.Error("reconcile: list labeled PRs", zap.String("label", label), zap.Error(err))
			continue
		}
		for _, issue := range issues {
			if _, ok := seen[issue.GetNumber()]; !ok {
				seen[issue.GetNumber()] = struct{}{}
				allIssues = append(allIssues, issue)
			}
		}
	}

	// Filter to PRs not already tracked in Redis and group by app.
	byApp := make(map[string][]recoveryCandidate)
	for _, issue := range allIssues {
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
			_ = s.store.ReleaseLock(ctx, c.meta.Environment, c.meta.App)

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
		":warning: Your deployment of *%s* (%s) `%s` (<%s|PR #%d>) was closed after a system restart.\n\nTo re-request: `/deploy %s` and select tag `%s`.",
		c.meta.App, c.meta.Environment, c.meta.Tag, c.prURL, c.number, c.meta.App, c.meta.Tag,
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
	slack   slackclient.Poster
	audit   audit.Logger
	metrics *metrics.Metrics
	cfg     *config.Holder
	log     *zap.Logger
}

func New(
	store *store.Store,
	gh *github.Client,
	slackClient slackclient.Poster,
	auditLog audit.Logger,
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
			_ = s.store.ReleaseLock(ctx, d.Environment, d.App)
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

		// Post expiry notice to the deploy channel, @mentioning the requester if available.
		deployChannel := s.cfg.Load().Slack.DeployChannel
		requester := "deploy-bot (ECR)"
		if d.RequesterID != "" {
			requester = fmt.Sprintf("<@%s>", d.RequesterID)
		}
		_, _, err = s.slack.PostMessageContext(ctx, deployChannel,
			slack.MsgOptionText(fmt.Sprintf(
				"Deployment of *%s* (%s) `%s` (<%s|PR #%d>) *expired* after %s with no approval. Requested by %s.",
				d.App, d.Environment, d.Tag, d.PRURL, d.PRNumber, staleDurationStr, requester,
			), false),
		)
		if err != nil {
			s.log.Error("post expiry notice", zap.Error(err))
		}

		_ = s.audit.Log(ctx, audit.AuditEvent{
			EventType:   audit.EventExpired,
			App:         d.App,
			Environment: d.Environment,
			Tag:         d.Tag,
			PRNumber:    d.PRNumber,
			PRURL:       d.PRURL,
			Requester:   d.Requester,
		})

		s.metrics.RecordDeploy(d.App, audit.EventExpired)
		_ = s.store.PushHistory(ctx, store.HistoryEntry{
			EventType:   audit.EventExpired,
			App:         d.App,
			Environment: d.Environment,
			Tag:         d.Tag,
			PRNumber:    d.PRNumber,
			PRURL:       d.PRURL,
			RequesterID: d.RequesterID,
			CompletedAt: time.Now(),
		})
		_ = s.store.ReleaseLock(ctx, d.Environment, d.App)
		_ = s.store.Delete(ctx, d.PRNumber)
	}

	// Refresh the pending gauge after each sweep pass.
	remaining, err := s.store.GetAll(ctx)
	if err == nil {
		s.metrics.SetPendingDeploys(len(remaining))
	}
}
