package approvers

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/go-github/v60/github"
	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
)

const membersKey = "team_members"

// Cache maintains a set of Slack user IDs who are authorized to interact with
// the bot. The set is stored in Redis so it is shared across replicas and
// survives pod restarts. It is refreshed periodically in the background.
//
// Sources are combined with OR logic:
//   - authorization.github_teams: GitHub team members resolved to Slack IDs
//   - authorization.github_users: GitHub users resolved to Slack IDs
//   - authorization.slack_user_groups: Slack user group members
//   - authorization.slack_emails: email addresses resolved to Slack user IDs
//
// A stale or incomplete cache fails open: unknown users are treated as
// non-members and the deploy modal returns an inline error. The live
// IsMember check in the worker remains the authoritative gate.
type Cache struct {
	rdb   *redis.Client
	gh    *github.Client
	slack *slack.Client
	auth  config.ParsedAuthorization
	org   string
	log   *zap.Logger
}

// New creates a Cache. The httpClient may be nil if no GitHub team is configured.
func New(httpClient *http.Client, slackClient *slack.Client, rdb *redis.Client, org string, auth config.ParsedAuthorization, log *zap.Logger) *Cache {
	var gh *github.Client
	if httpClient != nil {
		gh = github.NewClient(httpClient)
	}
	return &Cache{
		rdb:   rdb,
		gh:    gh,
		slack: slackClient,
		auth:  auth,
		org:   org,
		log:   log,
	}
}

// IsMember returns true if the Slack user ID is in the cached member set.
func (c *Cache) IsMember(slackUserID string) bool {
	ok, err := c.rdb.SIsMember(context.Background(), membersKey, slackUserID).Result()
	if err != nil {
		c.log.Warn("team cache: redis read failed, failing open", zap.Error(err))
		return false
	}
	return ok
}

// Refresh fetches all configured authorization sources and rebuilds the
// Slack user ID set in Redis. Errors from individual sources are logged
// but do not prevent other sources from being collected.
func (c *Cache) Refresh(ctx context.Context) error {
	var (
		mu   sync.Mutex
		seen = map[string]struct{}{}
		ids  []string
	)

	addID := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}

	var wg sync.WaitGroup

	// Source 1: Slack emails (resolved to Slack user IDs).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, email := range c.auth.SlackEmails {
			slackUser, err := c.slack.GetUserByEmailContext(ctx, email)
			if err != nil {
				c.log.Warn("team cache: could not resolve slack email",
					zap.String("email", email), zap.Error(err))
				continue
			}
			addID(slackUser.ID)
		}
	}()

	// Source 2: Slack user group members.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, groupID := range c.auth.SlackUserGroups {
			members, err := c.slack.GetUserGroupMembersContext(ctx, groupID)
			if err != nil {
				c.log.Warn("team cache: could not fetch slack user group members",
					zap.String("group", groupID), zap.Error(err))
				continue
			}
			for _, m := range members {
				addID(m)
			}
		}
	}()

	// Source 3: GitHub users (resolved to Slack IDs).
	if c.gh != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, login := range c.auth.GitHubUsers {
				slackID, email, err := c.resolveSlackID(ctx, login)
				if err != nil {
					c.log.Warn("team cache: could not resolve github user",
						zap.String("login", login), zap.String("email", email), zap.Error(err))
					continue
				}
				addID(slackID)
			}
		}()
	}

	// Source 4: GitHub team members (resolved to Slack IDs).
	if c.gh != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, team := range c.auth.GitHubTeams {
				ghMembers, err := c.fetchTeamMembers(ctx, team)
				if err != nil {
					c.log.Warn("team cache: could not fetch github team members",
						zap.String("team", team), zap.Error(err))
					continue
				}
				for _, login := range ghMembers {
					slackID, email, err := c.resolveSlackID(ctx, login)
					if err != nil {
						c.log.Warn("team cache: could not resolve team member",
							zap.String("email", email), zap.Error(err))
						continue
					}
					addID(slackID)
				}
			}
		}()
	}

	wg.Wait()

	if len(ids) > 0 {
		pipe := c.rdb.Pipeline()
		pipe.Del(ctx, membersKey)
		pipe.SAdd(ctx, membersKey, strSliceToAny(ids)...)
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("write member set to redis: %w", err)
		}
	}

	c.log.Info("team membership cache refreshed", zap.Int("count", len(ids)))
	return nil
}

func strSliceToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// StartRefresh calls Refresh on the given interval until ctx is cancelled.
func (c *Cache) StartRefresh(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.Refresh(ctx); err != nil {
					c.log.Error("team membership cache refresh failed", zap.Error(err))
				}
			}
		}
	}()
}

// fetchTeamMembers returns the GitHub logins of all active team members.
func (c *Cache) fetchTeamMembers(ctx context.Context, teamSlug string) ([]string, error) {
	var logins []string
	opts := &github.TeamListTeamMembersOptions{
		Role:        "all",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for {
		members, resp, err := c.gh.Teams.ListTeamMembersBySlug(ctx, c.org, teamSlug, opts)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			logins = append(logins, m.GetLogin())
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return logins, nil
}

// resolveSlackID maps a GitHub login to a Slack user ID via GitHub profile
// email → Slack user lookup by email. Returns the email alongside the Slack
// ID for logging purposes.
func (c *Cache) resolveSlackID(ctx context.Context, githubLogin string) (slackID, email string, err error) {
	ghUser, _, err := c.gh.Users.Get(ctx, githubLogin)
	if err != nil {
		return "", "", fmt.Errorf("could not find github user for email: %w", err)
	}
	email = ghUser.GetEmail()
	if email == "" {
		return "", "", fmt.Errorf("github user has no public email")
	}
	slackUser, err := c.slack.GetUserByEmailContext(ctx, email)
	if err != nil {
		return "", email, fmt.Errorf("could not find slack user for email: %w", err)
	}
	return slackUser.ID, email, nil
}
