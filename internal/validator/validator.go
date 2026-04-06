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

type cachedIdentity struct {
	GitHubLogin string `json:"github_login"`
	Email       string `json:"email"`
}

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

// SlackUserToGitHub resolves a Slack user ID to their GitHub login and email.
// Results are cached in Redis (no expiry) to avoid repeated Slack and GitHub
// API calls. For users mapped via github.users config, the cache is bypassed.
func (v *Validator) SlackUserToGitHub(ctx context.Context, slackUserID string) (login string, email string, err error) {
	if l, ok := v.cfg.GitHub.Users[slackUserID]; ok {
		return l, "", nil
	}

	// Check Redis cache.
	key := identityPrefix + slackUserID
	if data, err := v.rdb.Get(ctx, key).Bytes(); err == nil {
		var cached cachedIdentity
		if json.Unmarshal(data, &cached) == nil {
			return cached.GitHubLogin, cached.Email, nil
		}
	}

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

	// Cache in Redis with no expiry.
	if data, err := json.Marshal(cachedIdentity{GitHubLogin: login, Email: email}); err == nil {
		v.rdb.Set(ctx, key, data, 0)
	}

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
