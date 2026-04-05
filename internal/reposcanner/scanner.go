package reposcanner

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v60/github"
	"go.uber.org/zap"
	"golang.org/x/oauth2"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/slackclient"
)

// Scanner periodically scans GitHub repos for deploy-bot config files and
// updates the discovered apps list.
type Scanner struct {
	gh           *gh.Client
	org          string
	cfg          *config.Holder
	slack        slackclient.Poster
	cmWriter     ConfigMapWriter
	log          *zap.Logger

	// etags caches the ETag for each repo's config file to avoid re-fetching
	// unchanged files. Keyed by "owner/repo".
	etags map[string]string
	// lastContent caches the raw content for each repo, used when the ETag
	// matches (304). Keyed by "owner/repo".
	lastContent map[string][]byte
	// repoPushedAt tracks the last pushed_at time for each repo to skip
	// repos that haven't changed since the last scan.
	repoPushedAt map[string]time.Time

	conflict *conflictTracker
}

// ConfigMapWriter abstracts the Kubernetes ConfigMap update.
type ConfigMapWriter interface {
	Write(ctx context.Context, namespace, name, key string, data []byte) (changed bool, err error)
}

// NewScanner creates a Scanner. The cmWriter may be nil if ConfigMap writing
// is not needed (e.g. in tests).
func NewScanner(token, org string, slack slackclient.Poster, cmWriter ConfigMapWriter, cfg *config.Holder, log *zap.Logger) *Scanner {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	httpClient := oauth2.NewClient(context.Background(), ts)
	return &Scanner{
		gh:           gh.NewClient(httpClient),
		org:          org,
		cfg:          cfg,
		slack:        slack,
		cmWriter:     cmWriter,
		log:          log,
		etags:        make(map[string]string),
		lastContent:  make(map[string][]byte),
		repoPushedAt: make(map[string]time.Time),
		conflict:     newConflictTracker(),
	}
}

// Run starts the scan loop. It blocks until ctx is cancelled.
func (s *Scanner) Run(ctx context.Context) {
	c := s.cfg.Load()
	interval := c.RepoDiscovery.PollIntervalDuration()
	s.log.Info("reposcanner: starting",
		zap.String("org", s.org),
		zap.Duration("interval", interval),
		zap.String("config_file", c.RepoDiscovery.ConfigFileName()),
	)

	// Initial scan immediately.
	s.scan(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.log.Info("reposcanner: stopped")
			return
		case <-ticker.C:
			s.scan(ctx)
		}
	}
}

func (s *Scanner) scan(ctx context.Context) {
	c := s.cfg.Load()
	rd := c.RepoDiscovery
	configFile := rd.ConfigFileName()
	prefix := rd.RepoPrefix

	repos, err := s.listRepos(ctx, prefix)
	if err != nil {
		s.log.Error("reposcanner: list repos", zap.Error(err))
		return
	}

	var allDiscovered []config.DiscoveredAppConfig
	// Track which repos contributed apps for stale detection.
	seenRepos := make(map[string]bool)

	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}

		repoFullName := fmt.Sprintf("%s/%s", s.org, repo.GetName())
		seenRepos[repoFullName] = true

		// Check rate limit floor.
		if !s.checkRateLimit(ctx, rd.RateLimitFloorValue()) {
			s.log.Warn("reposcanner: rate limit floor reached, pausing scan")
			return
		}

		// Skip repos that haven't been pushed to since last scan.
		pushedAt := repo.GetPushedAt().Time
		if lastPush, ok := s.repoPushedAt[repoFullName]; ok && !pushedAt.After(lastPush) {
			// Use cached content if available.
			if cached, ok := s.lastContent[repoFullName]; ok {
				apps, errs := parseRepoConfig(cached, repoFullName)
				for _, e := range errs {
					s.log.Warn("reposcanner: validation error (cached)", zap.String("repo", repoFullName), zap.Error(e))
				}
				allDiscovered = append(allDiscovered, apps...)
			}
			continue
		}
		s.repoPushedAt[repoFullName] = pushedAt

		content, err := s.fetchConfigFile(ctx, repo, configFile)
		if err != nil {
			s.log.Debug("reposcanner: fetch config", zap.String("repo", repoFullName), zap.Error(err))
			// If we have cached content and this is a transient error, keep using it.
			if cached, ok := s.lastContent[repoFullName]; ok {
				apps, _ := parseRepoConfig(cached, repoFullName)
				allDiscovered = append(allDiscovered, apps...)
			}
			continue
		}
		if content == nil {
			// No config file in this repo.
			delete(s.lastContent, repoFullName)
			continue
		}

		s.lastContent[repoFullName] = content
		apps, errs := parseRepoConfig(content, repoFullName)
		for _, e := range errs {
			s.log.Warn("reposcanner: validation error", zap.String("repo", repoFullName), zap.Error(e))
		}
		allDiscovered = append(allDiscovered, apps...)
	}

	// Remove cached data for repos that are no longer visible.
	for repoName := range s.lastContent {
		if !seenRepos[repoName] {
			delete(s.lastContent, repoName)
			delete(s.etags, repoName)
			delete(s.repoPushedAt, repoName)
		}
	}

	// Detect conflicts with operator config.
	conflicts := s.detectConflicts(c, allDiscovered)
	warnChannel := rd.WarnChannel
	if warnChannel == "" {
		warnChannel = c.Slack.DeployChannel
	}
	s.conflict.emitWarnings(ctx, s.slack, warnChannel, configFile, conflicts)
	s.setCommitStatuses(ctx, allDiscovered, conflicts)

	// Filter out conflicting entries.
	var filtered []config.DiscoveredAppConfig
	for _, d := range allDiscovered {
		key := d.App + "\x00" + d.Environment
		if _, conflicting := conflicts[key]; !conflicting {
			filtered = append(filtered, d)
		}
	}

	// Write to ConfigMap.
	if s.cmWriter != nil {
		s.writeConfigMap(ctx, c, filtered)
	}

	s.log.Info("reposcanner: scan complete",
		zap.Int("repos_scanned", len(repos)),
		zap.Int("apps_discovered", len(filtered)),
		zap.Int("conflicts", len(conflicts)),
	)
}

func (s *Scanner) listRepos(ctx context.Context, prefix string) ([]*gh.Repository, error) {
	var allRepos []*gh.Repository
	opts := &gh.RepositoryListByOrgOptions{
		Sort:        "pushed",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for {
		repos, resp, err := s.gh.Repositories.ListByOrg(ctx, s.org, opts)
		if err != nil {
			return nil, fmt.Errorf("list repos: %w", err)
		}
		for _, r := range repos {
			if r.GetArchived() {
				continue
			}
			if prefix != "" && !strings.HasPrefix(r.GetName(), prefix) {
				continue
			}
			allRepos = append(allRepos, r)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return allRepos, nil
}

func (s *Scanner) fetchConfigFile(ctx context.Context, repo *gh.Repository, configFile string) ([]byte, error) {
	repoFullName := fmt.Sprintf("%s/%s", s.org, repo.GetName())
	defaultBranch := repo.GetDefaultBranch()
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	fc, _, resp, err := s.gh.Repositories.GetContents(ctx, s.org, repo.GetName(), configFile,
		&gh.RepositoryContentGetOptions{Ref: defaultBranch})

	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			return nil, nil // No config file — not an error.
		}
		return nil, fmt.Errorf("get contents: %w", err)
	}

	// Cache ETag for future conditional requests.
	if resp != nil && resp.Header.Get("ETag") != "" {
		s.etags[repoFullName] = resp.Header.Get("ETag")
	}

	if fc == nil {
		return nil, nil
	}

	content, err := fc.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decode content: %w", err)
	}
	return []byte(content), nil
}

func (s *Scanner) checkRateLimit(ctx context.Context, floor int) bool {
	limits, _, err := s.gh.RateLimit.Get(ctx)
	if err != nil {
		// If we can't check, proceed optimistically.
		return true
	}
	return limits.Core.Remaining >= floor
}

// detectConflicts returns a map of "app\x00environment" keys that exist in
// both operator config and discovered apps.
func (s *Scanner) detectConflicts(c *config.Config, discovered []config.DiscoveredAppConfig) map[string]conflictInfo {
	operatorApps := make(map[string]struct{}, len(c.Apps))
	operatorPaths := make(map[string]string, len(c.Apps)) // kustomize_path -> "app (env)"
	for _, a := range c.Apps {
		operatorApps[a.App+"\x00"+a.Environment] = struct{}{}
		if a.KustomizePath != "" {
			operatorPaths[a.KustomizePath] = fmt.Sprintf("%s (%s)", a.App, a.Environment)
		}
	}

	conflicts := make(map[string]conflictInfo)
	// Track kustomize_paths claimed by non-conflicting discovered apps.
	discoveredPaths := make(map[string]string) // kustomize_path -> "app (env) from repo"

	for _, d := range discovered {
		key := d.App + "\x00" + d.Environment
		if _, ok := conflicts[key]; ok {
			continue
		}

		// Check app+environment conflict with operator config.
		if _, ok := operatorApps[key]; ok {
			conflicts[key] = conflictInfo{
				App:        d.App,
				Env:        d.Environment,
				SourceRepo: d.SourceRepo,
				Reason:     "app+environment",
			}
			continue
		}

		// Check kustomize_path conflict with operator config.
		if d.KustomizePath != "" {
			if other, ok := operatorPaths[d.KustomizePath]; ok {
				conflicts[key] = conflictInfo{
					App:        d.App,
					Env:        d.Environment,
					SourceRepo: d.SourceRepo,
					Reason:     "kustomize_path",
					Detail:     fmt.Sprintf("operator app %s", other),
				}
				continue
			}

			// Check kustomize_path conflict with other discovered apps.
			label := fmt.Sprintf("%s (%s) from %s", d.App, d.Environment, d.SourceRepo)
			if other, ok := discoveredPaths[d.KustomizePath]; ok {
				conflicts[key] = conflictInfo{
					App:        d.App,
					Env:        d.Environment,
					SourceRepo: d.SourceRepo,
					Reason:     "kustomize_path",
					Detail:     other,
				}
				continue
			}
			discoveredPaths[d.KustomizePath] = label
		}
	}
	return conflicts
}

func (s *Scanner) writeConfigMap(ctx context.Context, c *config.Config, apps []config.DiscoveredAppConfig) {
	rd := c.RepoDiscovery
	da := config.DiscoveredApps{Apps: apps}

	data, err := marshalDiscoveredApps(da)
	if err != nil {
		s.log.Error("reposcanner: marshal discovered apps", zap.Error(err))
		return
	}

	ns := rd.ConfigMapNamespace
	name := rd.ConfigMapTargetName()
	changed, err := s.cmWriter.Write(ctx, ns, name, "discovered.json", data)
	if err != nil {
		s.log.Error("reposcanner: write configmap", zap.Error(err))
		return
	}
	if changed {
		s.log.Info("reposcanner: configmap updated",
			zap.String("name", name),
			zap.Int("apps", len(apps)),
		)
	}
}

// setCommitStatuses sets commit statuses on repos with discovered apps.
func (s *Scanner) setCommitStatuses(ctx context.Context, discovered []config.DiscoveredAppConfig, conflicts map[string]conflictInfo) {
	// Group conflicts by repo.
	repoConflicts := make(map[string][]conflictInfo)
	for _, c := range conflicts {
		repoConflicts[c.SourceRepo] = append(repoConflicts[c.SourceRepo], c)
	}

	// Group all discovered by repo.
	repoApps := make(map[string]bool)
	for _, d := range discovered {
		repoApps[d.SourceRepo] = true
	}

	for repoFullName := range repoApps {
		parts := strings.SplitN(repoFullName, "/", 2)
		if len(parts) != 2 {
			continue
		}
		owner, repo := parts[0], parts[1]

		var state, description string
		if cList, ok := repoConflicts[repoFullName]; ok {
			state = "failure"
			var descs []string
			for _, c := range cList {
				switch c.Reason {
				case "kustomize_path":
					descs = append(descs, fmt.Sprintf("%s (%s): path conflict with %s", c.App, c.Env, c.Detail))
				default:
					descs = append(descs, fmt.Sprintf("%s (%s): defined in operator config", c.App, c.Env))
				}
			}
			description = fmt.Sprintf("Conflict: %s", strings.Join(descs, "; "))
			if len(description) > 140 {
				description = description[:137] + "..."
			}
		} else {
			state = "success"
			description = "All apps registered successfully"
		}

		// Get the default branch HEAD SHA.
		ref, _, err := s.gh.Git.GetRef(ctx, owner, repo, "refs/heads/main")
		if err != nil {
			s.log.Debug("reposcanner: get ref for status", zap.String("repo", repoFullName), zap.Error(err))
			continue
		}

		statusCtx := "deploy-bot/config"
		_, _, err = s.gh.Repositories.CreateStatus(ctx, owner, repo, ref.GetObject().GetSHA(), &gh.RepoStatus{
			State:       &state,
			Description: &description,
			Context:     &statusCtx,
		})
		if err != nil {
			s.log.Warn("reposcanner: set commit status", zap.String("repo", repoFullName), zap.Error(err))
		}
	}
}
