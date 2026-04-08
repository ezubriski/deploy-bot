package validator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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

// String returns the identity in "Name (email)" format for logs and audit.
func (id Identity) String() string {
	if id.Name != "" && id.Email != "" {
		return id.Name + " (" + id.Email + ")"
	}
	if id.Email != "" {
		return id.Email
	}
	if id.Name != "" {
		return id.Name
	}
	return ""
}

type cachedIdentity = Identity

type Validator struct {
	gh    *github.Client
	slack *slack.Client
	rdb   *redis.Client
	cfg   *config.Config
	auth  config.ParsedAuthorization
	log   *zap.Logger
}

func New(httpClient *http.Client, slackClient *slack.Client, rdb *redis.Client, cfg *config.Config, log *zap.Logger) *Validator {
	ghTeams, ghUsers, slackGroups, slackEmails, err := config.ParseAuthValues(cfg.Authorization)
	if err != nil {
		// ParseAuthValues errors here are configuration mistakes; the receiver
		// validates the same config at startup and exits on error, so this
		// branch is reachable only in tests or partial configs. Log loudly
		// instead of dropping silently.
		log.Error("validator: parse authorization config", zap.Error(err))
	}
	return &Validator{
		gh:    github.NewClient(httpClient),
		slack: slackClient,
		rdb:   rdb,
		cfg:   cfg,
		auth: config.ParsedAuthorization{
			GitHubTeams:     ghTeams,
			GitHubUsers:     ghUsers,
			SlackUserGroups: slackGroups,
			SlackEmails:     slackEmails,
		},
		log: log,
	}
}

// ResolveIdentity resolves a Slack user ID to their full identity (GitHub login,
// email, display name). Results are cached in Redis (no expiry) to avoid
// repeated Slack and GitHub API calls.
//
// Always attempts to resolve email and name from Slack. The GitHub login is
// taken from `identity_overrides` if set; otherwise resolved via GitHub email
// search. A partial identity (email/name only) is returned when GitHub
// resolution fails.
func (v *Validator) ResolveIdentity(ctx context.Context, slackUserID string) (Identity, error) {
	// Check Redis cache.
	key := identityPrefix + slackUserID
	if data, err := v.rdb.Get(ctx, key).Bytes(); err == nil {
		var cached cachedIdentity
		if json.Unmarshal(data, &cached) == nil && cached.Email != "" {
			return cached, nil
		}
	}

	// Resolve email and name via Slack API.
	info, err := v.slack.GetUserInfoContext(ctx, slackUserID)
	if err != nil {
		return Identity{}, fmt.Errorf("get slack user info: %w", err)
	}
	email := info.Profile.Email
	name := info.Profile.RealName
	if email == "" {
		return Identity{Name: name}, fmt.Errorf("slack user %s has no email", slackUserID)
	}

	id := Identity{Email: email, Name: name}

	// Identity override takes precedence over GitHub email search.
	if login, ok := v.cfg.IdentityOverrides[slackUserID]; ok {
		id.GitHubLogin = login
	} else {
		result, _, err := v.gh.Search.Users(ctx, fmt.Sprintf("%s in:email", email), nil)
		if err != nil {
			v.log.Warn("identity: github search failed",
				zap.String("email", email), zap.Error(err))
		} else if result.GetTotal() == 0 || len(result.Users) == 0 {
			v.log.Warn("identity: no github user found for email",
				zap.String("email", email))
		} else {
			id.GitHubLogin = result.Users[0].GetLogin()
		}
	}

	// Cache the identity. We cache partial identities (email/name without
	// GitHub login) so we don't keep retrying failed GitHub searches.
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

// IsMember checks if a Slack user is authorized via any configured source.
// Sources are checked with OR logic: Slack emails, Slack user groups,
// GitHub users, and GitHub teams.
func (v *Validator) IsMember(ctx context.Context, slackUserID string) (bool, Identity, error) {
	auth := v.auth

	// Slack emails: resolve the user's email and compare.
	if len(auth.SlackEmails) > 0 {
		info, err := v.slack.GetUserInfoContext(ctx, slackUserID)
		if err != nil {
			v.log.Warn("check slack email membership: could not get user info", zap.Error(err))
		} else if info.Profile.Email != "" {
			for _, email := range auth.SlackEmails {
				if strings.EqualFold(email, info.Profile.Email) {
					id, err := v.ResolveIdentity(ctx, slackUserID)
					if err != nil {
						v.log.Warn("resolve identity for matched member", zap.String("slack_user", slackUserID), zap.Error(err))
					}
					return true, id, nil
				}
			}
		}
	}

	// Slack user groups: check each group's member list.
	for _, groupID := range auth.SlackUserGroups {
		members, err := v.slack.GetUserGroupMembersContext(ctx, groupID)
		if err != nil {
			v.log.Warn("check slack user group membership", zap.String("group", groupID), zap.Error(err))
			continue
		}
		for _, m := range members {
			if m == slackUserID {
				id, err := v.ResolveIdentity(ctx, slackUserID)
				if err != nil {
					v.log.Warn("resolve identity for matched group member", zap.String("slack_user", slackUserID), zap.String("group", groupID), zap.Error(err))
				}
				return true, id, nil
			}
		}
	}

	// GitHub sources require identity resolution.
	if len(auth.GitHubUsers) > 0 || len(auth.GitHubTeams) > 0 {
		id, err := v.ResolveIdentity(ctx, slackUserID)
		if err != nil {
			return false, id, err
		}

		// Skip GitHub-based checks if we couldn't resolve a GitHub login
		// (private email, no GitHub account, or no identity_overrides entry).
		if id.GitHubLogin == "" {
			return false, id, nil
		}

		// GitHub users: direct login match.
		for _, login := range auth.GitHubUsers {
			if strings.EqualFold(login, id.GitHubLogin) {
				return true, id, nil
			}
		}

		// GitHub teams: check membership in each team.
		for _, team := range auth.GitHubTeams {
			ok, _, err := v.isTeamMember(ctx, id.GitHubLogin, team)
			if err != nil {
				v.log.Warn("check github team membership", zap.String("team", team), zap.Error(err))
				continue
			}
			if ok {
				return true, id, nil
			}
		}
		return false, id, nil
	}

	id, err := v.ResolveIdentity(ctx, slackUserID)
	if err != nil {
		v.log.Warn("resolve identity for non-member fallthrough", zap.String("slack_user", slackUserID), zap.Error(err))
	}
	return false, id, nil
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
