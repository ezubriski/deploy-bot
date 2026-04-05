package validator

import (
	"context"
	"fmt"

	"github.com/google/go-github/v60/github"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
	"golang.org/x/oauth2"

	"github.com/ezubriski/deploy-bot/internal/config"
)

type Validator struct {
	gh    *github.Client
	slack *slack.Client
	cfg   *config.Config
	log   *zap.Logger
}

func New(ghToken string, slackClient *slack.Client, cfg *config.Config, log *zap.Logger) *Validator {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: ghToken})
	httpClient := oauth2.NewClient(context.Background(), ts)
	ghClient := github.NewClient(httpClient)
	return &Validator{
		gh:    ghClient,
		slack: slackClient,
		cfg:   cfg,
		log:   log,
	}
}

// SlackUserToGitHub resolves a Slack user ID to their GitHub login.
// It first checks the github.users config map (for users with private GitHub
// emails), then falls back to searching GitHub by Slack profile email.
func (v *Validator) SlackUserToGitHub(ctx context.Context, slackUserID string) (string, error) {
	if login, ok := v.cfg.GitHub.Users[slackUserID]; ok {
		return login, nil
	}

	info, err := v.slack.GetUserInfoContext(ctx, slackUserID)
	if err != nil {
		return "", fmt.Errorf("get slack user info: %w", err)
	}
	email := info.Profile.Email
	if email == "" {
		return "", fmt.Errorf("slack user %s has no email", slackUserID)
	}

	result, _, err := v.gh.Search.Users(ctx, fmt.Sprintf("%s in:email", email), nil)
	if err != nil {
		return "", fmt.Errorf("search github user: %w", err)
	}
	if result.GetTotal() == 0 || len(result.Users) == 0 {
		return "", fmt.Errorf("no github user found for email %s", email)
	}
	return result.Users[0].GetLogin(), nil
}

// IsDeployer checks if a Slack user is a member of the deployer GitHub team.
func (v *Validator) IsDeployer(ctx context.Context, slackUserID string) (bool, string, error) {
	login, err := v.SlackUserToGitHub(ctx, slackUserID)
	if err != nil {
		return false, "", err
	}
	return v.isTeamMember(ctx, login, v.cfg.GitHub.DeployerTeam)
}

// IsApprover checks if a Slack user is a member of the approver GitHub team.
func (v *Validator) IsApprover(ctx context.Context, slackUserID string) (bool, string, error) {
	login, err := v.SlackUserToGitHub(ctx, slackUserID)
	if err != nil {
		return false, "", err
	}
	return v.isTeamMember(ctx, login, v.cfg.GitHub.ApproverTeam)
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
