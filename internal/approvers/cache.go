package approvers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-github/v60/github"
	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

const approversKey = "approvers"

// Cache maintains a set of Slack user IDs who are members of the GitHub
// approver team. The set is stored in Redis so it is shared across replicas
// and survives pod restarts. It is refreshed periodically in the background.
//
// A stale or incomplete cache fails open: unknown users are treated as
// non-approvers and the deploy modal returns an inline error. The live
// IsApprover check in the worker remains the authoritative gate.
type Cache struct {
	rdb      *redis.Client
	gh       *github.Client
	slack    *slack.Client
	org      string
	teamSlug string
	log      *zap.Logger
}

func New(httpClient *http.Client, slackClient *slack.Client, rdb *redis.Client, org, teamSlug string, log *zap.Logger) *Cache {
	return &Cache{
		rdb:      rdb,
		gh:       github.NewClient(httpClient),
		slack:    slackClient,
		org:      org,
		teamSlug: teamSlug,
		log:      log,
	}
}

// IsApprover returns true if the Slack user ID is in the cached approver set.
func (c *Cache) IsApprover(slackUserID string) bool {
	ok, err := c.rdb.SIsMember(context.Background(), approversKey, slackUserID).Result()
	if err != nil {
		c.log.Warn("approver cache: redis read failed, failing open", zap.Error(err))
		return false
	}
	return ok
}

// Refresh fetches the current GitHub team roster and rebuilds the Slack user
// ID set in Redis. Errors are logged but do not clear the existing set.
func (c *Cache) Refresh(ctx context.Context) error {
	members, err := c.fetchTeamMembers(ctx)
	if err != nil {
		return fmt.Errorf("fetch team members: %w", err)
	}

	ids := make([]string, 0, len(members))
	for _, login := range members {
		slackID, err := c.resolveSlackID(ctx, login)
		if err != nil {
			c.log.Warn("approver cache: could not resolve Slack ID",
				zap.String("github_login", login), zap.Error(err))
			continue
		}
		ids = append(ids, slackID)
	}

	if len(ids) > 0 {
		pipe := c.rdb.Pipeline()
		pipe.Del(ctx, approversKey)
		pipe.SAdd(ctx, approversKey, strSliceToAny(ids)...)
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("write approver set to redis: %w", err)
		}
	}

	c.log.Info("approver cache refreshed", zap.Int("count", len(ids)))
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
					c.log.Error("approver cache refresh failed", zap.Error(err))
				}
			}
		}
	}()
}

// fetchTeamMembers returns the GitHub logins of all active team members.
func (c *Cache) fetchTeamMembers(ctx context.Context) ([]string, error) {
	var logins []string
	opts := &github.TeamListTeamMembersOptions{
		Role:        "all",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for {
		members, resp, err := c.gh.Teams.ListTeamMembersBySlug(ctx, c.org, c.teamSlug, opts)
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
// email → Slack user lookup by email.
func (c *Cache) resolveSlackID(ctx context.Context, githubLogin string) (string, error) {
	ghUser, _, err := c.gh.Users.Get(ctx, githubLogin)
	if err != nil {
		return "", fmt.Errorf("get github user %s: %w", githubLogin, err)
	}
	email := ghUser.GetEmail()
	if email == "" {
		return "", fmt.Errorf("github user %s has no public email", githubLogin)
	}
	slackUser, err := c.slack.GetUserByEmailContext(ctx, email)
	if err != nil {
		return "", fmt.Errorf("lookup slack user for %s: %w", email, err)
	}
	return slackUser.ID, nil
}
