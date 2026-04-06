package validator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/go-github/v60/github"
	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
)

const identityPrefix = "identity:"

// Identity holds the resolved identity of a Slack user.
type Identity struct {
	GitHubLogin string `json:"github_login"`
	Email       string `json:"email"`
	Name        string `json:"name"` // Slack profile display name (real name)
}

type cachedIdentity = Identity

type Validator struct {
	gh    *github.Client
	slack *slack.Client
	rdb   *redis.Client
	cfg   *config.Config
	log   *zap.Logger
}

func New(httpClient *http.Client, slackClient *slack.Client, rdb *redis.Client, cfg *config.Config, log *zap.Logger) *Validator {
	return &Validator{
		gh:    github.NewClient(httpClient),
		slack: slackClient,
		rdb:   rdb,
		cfg:   cfg,
		log:   log,
	}
}

// ResolveIdentity resolves a Slack user ID to their full identity (GitHub login,
// email, display name). Results are cached in Redis (no expiry) to avoid
// repeated Slack and GitHub API calls. For users mapped via github.users config,
// only the GitHub login is available (no Slack lookup needed).
func (v *Validator) ResolveIdentity(ctx context.Context, slackUserID string) (Identity, error) {
	if l, ok := v.cfg.GitHub.Users[slackUserID]; ok {
		return Identity{GitHubLogin: l}, nil
	}

	// Check Redis cache.
	key := identityPrefix + slackUserID
	if data, err := v.rdb.Get(ctx, key).Bytes(); err == nil {
		var cached cachedIdentity
		if json.Unmarshal(data, &cached) == nil && cached.GitHubLogin != "" {
			return cached, nil
		}
	}

	// Resolve via Slack API.
	info, err := v.slack.GetUserInfoContext(ctx, slackUserID)
	if err != nil {
		return Identity{}, fmt.Errorf("get slack user info: %w", err)
	}
	email := info.Profile.Email
	if email == "" {
		return Identity{}, fmt.Errorf("slack user %s has no email", slackUserID)
	}
	name := info.Profile.RealName

	// Resolve via GitHub API.
	result, _, err := v.gh.Search.Users(ctx, fmt.Sprintf("%s in:email", email), nil)
	if err != nil {
		return Identity{Email: email, Name: name}, fmt.Errorf("search github user: %w", err)
	}
	if result.GetTotal() == 0 || len(result.Users) == 0 {
		return Identity{Email: email, Name: name}, fmt.Errorf("no github user found for email %s", email)
	}

	id := Identity{
		GitHubLogin: result.Users[0].GetLogin(),
		Email:       email,
		Name:        name,
	}

	// Cache in Redis with no expiry.
	if data, err := json.Marshal(id); err == nil {
		v.rdb.Set(ctx, key, data, 0)
	}

	return id, nil
}

// SlackUserToGitHub is a convenience wrapper around ResolveIdentity that
// returns just the GitHub login. Kept for call sites that only need the login
// (e.g. GitHub PR comments).
func (v *Validator) SlackUserToGitHub(ctx context.Context, slackUserID string) (string, error) {
	id, err := v.ResolveIdentity(ctx, slackUserID)
	return id.GitHubLogin, err
}

// IsDeployer checks if a Slack user is a member of the deployer GitHub team.
func (v *Validator) IsDeployer(ctx context.Context, slackUserID string) (bool, Identity, error) {
	id, err := v.ResolveIdentity(ctx, slackUserID)
	if err != nil {
		return false, id, err
	}
	ok, _, err := v.isTeamMember(ctx, id.GitHubLogin, v.cfg.GitHub.DeployerTeam)
	return ok, id, err
}

// IsApprover checks if a Slack user is a member of the approver GitHub team.
func (v *Validator) IsApprover(ctx context.Context, slackUserID string) (bool, Identity, error) {
	id, err := v.ResolveIdentity(ctx, slackUserID)
	if err != nil {
		return false, id, err
	}
	ok, _, err := v.isTeamMember(ctx, id.GitHubLogin, v.cfg.GitHub.ApproverTeam)
	return ok, id, err
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
