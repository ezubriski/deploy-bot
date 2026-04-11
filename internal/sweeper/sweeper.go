package sweeper

import (
	"context"
	"fmt"
	"slices"
	"sync"
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

	var (
		mu  sync.Mutex
		all []github.DeployCommit
	)
	{
		var wg sync.WaitGroup
		sem := make(chan struct{}, 5)
		for _, app := range cfg.Apps {
			wg.Add(1)
			sem <- struct{}{}
			go func(app config.AppConfig) {
				defer wg.Done()
				defer func() { <-sem }()
				commits, err := s.gh.ListDeployCommits(ctx, app.KustomizePath, store.HistoryMaxLen)
				if err != nil {
					s.log.Warn("reconstruct history: list commits",
						zap.String("app", app.App), zap.Error(err))
					return
				}
				mu.Lock()
				all = append(all, commits...)
				mu.Unlock()
			}(app)
		}
		wg.Wait()
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

	// Resolve PR info for each commit in parallel; preserve order so the
	// subsequent LPUSH leaves history newest-first.
	type prInfo struct {
		number int
		url    string
	}
	prInfos := make([]prInfo, len(all))
	{
		var wg sync.WaitGroup
		sem := make(chan struct{}, 5)
		for i, c := range all {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, sha string) {
				defer wg.Done()
				defer func() { <-sem }()
				prNumber, prURL, err := s.gh.PRForCommit(ctx, sha)
				if err != nil {
					s.log.Warn("reconstruct history: lookup PR for commit",
						zap.String("sha", sha), zap.Error(err))
				}
				prInfos[i] = prInfo{number: prNumber, url: prURL}
			}(i, c.SHA)
		}
		wg.Wait()
	}

	pushed := 0
	for i, c := range all {
		entry := store.HistoryEntry{
			EventType:   audit.EventApproved,
			App:         c.App,
			Tag:         c.Tag,
			PRNumber:    prInfos[i].number,
			PRURL:       prInfos[i].url,
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

// ReconcileFromGitHub scans open PRs carrying the deploy-bot label (pending or
// base) and re-hydrates any that are missing from Redis. This recovers state
// after a Redis flush or when entries expire before the sweeper can act. PRs
// are re-inserted into Redis so they appear in /deploy list and can be
// cancelled or approved through normal flows.
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

	staleDuration := cfg.StaleDuration()
	now := time.Now()
	recovered := 0

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

		// Re-hydrate the pending deploy so it appears in /deploy list
		// and can be acted on through normal flows (cancel, approve).
		createdAt := issue.GetCreatedAt().Time
		expiresAt := createdAt.Add(staleDuration)
		if expiresAt.Before(now) {
			// Already past expiry — set a short TTL so the sweeper's
			// expiry pass picks it up on the next cycle.
			expiresAt = now.Add(time.Minute)
		}

		d := &store.PendingDeploy{
			App:         meta.App,
			Environment: meta.Environment,
			Tag:         meta.Tag,
			PRNumber:    prNumber,
			PRURL:       issue.GetHTMLURL(),
			Requester:   meta.RequesterSlackID,
			RequesterID: meta.RequesterSlackID,
			Reason:      "recovered by reconciler",
			RequestedAt: createdAt,
			ExpiresAt:   expiresAt,
			State:       store.StatePending,
		}

		ttl := time.Until(expiresAt)
		if ttl <= 0 {
			ttl = time.Minute
		}
		if err := s.store.Set(ctx, d, ttl); err != nil {
			s.log.Error("reconcile: re-hydrate deploy", zap.Int("pr", prNumber), zap.Error(err))
			continue
		}

		// Ensure the pending label is present so future sweeps can expire it.
		s.warnIfErr("github: add pending label", s.gh.AddLabels(ctx, prNumber, []string{cfg.PendingLabel()}), zap.Int("pr", prNumber))

		recovered++
		s.log.Info("reconcile: re-hydrated orphaned PR",
			zap.Int("pr", prNumber),
			zap.String("app", meta.App),
			zap.Time("expires", expiresAt),
		)
	}

	if recovered > 0 {
		s.log.Info("reconcile: complete", zap.Int("recovered", recovered))
	}
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

// warnIfErr logs err at Warn for recoverable failures (stale label,
// missed comment, undelivered notice). No-op when err is nil.
func (s *Sweeper) warnIfErr(op string, err error, fields ...zap.Field) {
	if err == nil {
		return
	}
	s.log.Warn(op, append(fields, zap.Error(err))...)
}

// errIfErr is the Error-level counterpart for failures that leave orphan
// state or lose audit records. See bot.Bot.errIfErr for the policy.
func (s *Sweeper) errIfErr(op string, err error, fields ...zap.Field) {
	if err == nil {
		return
	}
	s.log.Error(op, append(fields, zap.Error(err))...)
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
			// The recovered SHA is intentionally discarded here: this code
			// path runs at leader startup and predates the history-push
			// goroutines for normal merges. A recovered deploy is logged
			// but does not produce a history entry, so there is nothing to
			// correlate ArgoCD signals against from this path.
			if _, err := s.gh.MergePR(ctx, d.PRNumber, s.cfg.Load().Deployment.MergeMethod); err != nil {
				s.log.Error("recover merge failed", zap.Int("pr", d.PRNumber), zap.Error(err))
				continue
			}
			s.warnIfErr("github: remove pending label", s.gh.RemoveLabel(ctx, d.PRNumber, s.cfg.Load().PendingLabel()), zap.Int("pr", d.PRNumber))
			s.errIfErr("store: release lock", s.store.ReleaseLock(ctx, d.Environment, d.App), zap.String("env", d.Environment), zap.String("app", d.App))
			s.errIfErr("store: delete pending", s.store.Delete(ctx, d.PRNumber), zap.Int("pr", d.PRNumber))
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

	staleDuration := s.cfg.Load().StaleDuration()
	staleDurationStr := fmt.Sprintf("%v", staleDuration)

	cfg := s.cfg.Load()
	for _, d := range expired {
		s.log.Info("expiring deployment", zap.Int("pr", d.PRNumber), zap.String("app", d.App))

		requester := "deploy-bot (ECR)"
		if d.RequesterID != "" {
			requester = fmt.Sprintf("<@%s>", d.RequesterID)
		}

		var wg sync.WaitGroup
		wg.Add(7)
		go func() {
			defer wg.Done()
			s.warnIfErr("github: comment expired", s.gh.CommentExpired(ctx, d.PRNumber, staleDurationStr), zap.Int("pr", d.PRNumber))
		}()
		go func() {
			defer wg.Done()
			s.warnIfErr("github: close PR", s.gh.ClosePR(ctx, d.PRNumber), zap.Int("pr", d.PRNumber))
		}()
		go func() {
			defer wg.Done()
			s.warnIfErr("github: remove pending label", s.gh.RemoveLabel(ctx, d.PRNumber, cfg.PendingLabel()), zap.Int("pr", d.PRNumber))
		}()
		go func() {
			defer wg.Done()
			s.errIfErr("store: release lock", s.store.ReleaseLock(ctx, d.Environment, d.App), zap.String("env", d.Environment), zap.String("app", d.App))
		}()
		go func() {
			defer wg.Done()
			s.errIfErr("store: delete pending", s.store.Delete(ctx, d.PRNumber), zap.Int("pr", d.PRNumber))
		}()
		go func() {
			defer wg.Done()
			if _, _, err := s.slack.PostMessageContext(ctx, cfg.Slack.DeployChannel,
				slack.MsgOptionText(fmt.Sprintf(
					"Deployment of *%s* (%s) `%s` (<%s|PR #%d>) *expired* after %s with no approval. Requested by %s.",
					d.App, d.Environment, d.Tag, d.PRURL, d.PRNumber, staleDurationStr, requester,
				), false),
			); err != nil {
				s.log.Warn("slack post: expired notice", zap.Int("pr", d.PRNumber), zap.Error(err))
			}
		}()
		go func() {
			defer wg.Done()
			if err := s.audit.Log(ctx, audit.AuditEvent{
				EventType:   audit.EventExpired,
				Trigger:     audit.TriggerSweeper,
				App:         d.App,
				Environment: d.Environment,
				Tag:         d.Tag,
				PRNumber:    d.PRNumber,
				PRURL:       d.PRURL,
				Reason:      "stale duration exceeded",
			}); err != nil {
				s.log.Error("audit log", zap.Error(err))
			}
		}()
		s.metrics.RecordDeploy(d.App, audit.EventExpired)
		if err := s.store.PushHistory(ctx, store.HistoryEntry{
			EventType:      audit.EventExpired,
			App:            d.App,
			Environment:    d.Environment,
			Tag:            d.Tag,
			PRNumber:       d.PRNumber,
			PRURL:          d.PRURL,
			RequesterID:    d.RequesterID,
			CompletedAt:    time.Now(),
			SlackChannel:   d.SlackChannel,
			SlackMessageTS: d.SlackMessageTS,
		}); err != nil {
			s.log.Warn("store: push history", zap.Error(err))
		}
		wg.Wait()
	}

	// Close any orphaned PRs whose Redis entries have already expired.
	s.ReconcileFromGitHub(ctx)

	// Refresh the pending gauge after each sweep pass.
	remaining, err := s.store.GetAll(ctx)
	if err == nil {
		s.metrics.SetPendingDeploys(len(remaining))
	}
}
