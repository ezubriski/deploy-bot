package validator

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/go-github/v60/github"
	"github.com/slack-go/slack"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
)

// identityCacheTTL controls how long resolved Slack → GitHub identity mappings
// are cached. User email and GitHub login change infrequently.
const identityCacheTTL = 15 * time.Minute

type cachedIdentity struct {
	ghLogin string
	email   string
	expires time.Time
}

type Validator struct {
	gh    *github.Client
	slack *slack.Client
	cfg   *config.Config
	log   *zap.Logger

	mu    sync.RWMutex
	cache map[string]cachedIdentity // keyed by Slack user ID
}

func New(httpClient *http.Client, slackClient *slack.Client, cfg *config.Config, log *zap.Logger) *Validator {
	return &Validator{
		gh:    github.NewClient(httpClient),
		slack: slackClient,
		cfg:   cfg,
		log:   log,
		cache: make(map[string]cachedIdentity),
	}
}

// SlackUserToGitHub resolves a Slack user ID to their GitHub login and email.
// Results are cached for 15 minutes to reduce Slack and GitHub API calls.
// For users mapped via github.users config, the cache is bypassed (no API calls needed).
func (v *Validator) SlackUserToGitHub(ctx context.Context, slackUserID string) (login string, email string, err error) {
	if l, ok := v.cfg.GitHub.Users[slackUserID]; ok {
		return l, "", nil
	}

	// Check cache.
	v.mu.RLock()
	if cached, ok := v.cache[slackUserID]; ok && time.Now().Before(cached.expires) {
		v.mu.RUnlock()
		return cached.ghLogin, cached.email, nil
	}
	v.mu.RUnlock()

	// Resolve via Slack API.
	info, err := v.slack.GetUserInfoContext(ctx, slackUserID)
	if err != nil {
		return "", "", fmt.Errorf("get slack user info: %w", err)
	}
	email = info.Profile.Email
	if email == "" {
		return "", "", fmt.Errorf("slack user %s has no email", slackUserID)
	}

	// Resolve via GitHub API.
	result, _, err := v.gh.Search.Users(ctx, fmt.Sprintf("%s in:email", email), nil)
	if err != nil {
		return "", email, fmt.Errorf("search github user: %w", err)
	}
	if result.GetTotal() == 0 || len(result.Users) == 0 {
		return "", email, fmt.Errorf("no github user found for email %s", email)
	}

	login = result.Users[0].GetLogin()

	// Cache the result.
	v.mu.Lock()
	v.cache[slackUserID] = cachedIdentity{
		ghLogin: login,
		email:   email,
		expires: time.Now().Add(identityCacheTTL),
	}
	v.mu.Unlock()

	return login, email, nil
}

// IsDeployer checks if a Slack user is a member of the deployer GitHub team.
// Returns the GitHub login and Slack profile email alongside the membership check.
func (v *Validator) IsDeployer(ctx context.Context, slackUserID string) (bool, string, string, error) {
	login, email, err := v.SlackUserToGitHub(ctx, slackUserID)
	if err != nil {
		return false, "", email, err
	}
	ok, ghLogin, err := v.isTeamMember(ctx, login, v.cfg.GitHub.DeployerTeam)
	return ok, ghLogin, email, err
}

// IsApprover checks if a Slack user is a member of the approver GitHub team.
// Returns the GitHub login and Slack profile email alongside the membership check.
func (v *Validator) IsApprover(ctx context.Context, slackUserID string) (bool, string, string, error) {
	login, email, err := v.SlackUserToGitHub(ctx, slackUserID)
	if err != nil {
		return false, "", email, err
	}
	ok, ghLogin, err := v.isTeamMember(ctx, login, v.cfg.GitHub.ApproverTeam)
	return ok, ghLogin, email, err
}

func (v *Validator) isTeamMember(ctx context.Context, githubLogin, teamSlug string) (bool, string, error) {
	membership, resp, err := v.gh.Teams.GetTeamMembershipBySlug(ctx, v.cfg.GitHub.Org, teamSlug, githubLogin)
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			return false, githubLogin, nil
		}
		return false, githubLogin, fmt.Errorf("get team membership: %w", err)
	}
	return membership.GetState() == "active", githubLogin, nil
}
