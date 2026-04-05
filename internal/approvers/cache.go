package approvers

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/go-github/v60/github"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

// Cache maintains a set of Slack user IDs who are members of the GitHub
// approver team. It is built by mapping GitHub logins → GitHub email →
// Slack user ID, and refreshed periodically in the background.
//
// A stale or incomplete cache fails open: unknown users are treated as
// non-approvers and the deploy modal returns an inline error. The live
// IsApprover check in the worker remains the authoritative gate.
type Cache struct {
	mu          sync.RWMutex
	approverIDs map[string]struct{} // keyed by Slack user ID

	gh       *github.Client
	slack    *slack.Client
	org      string
	teamSlug string
	log      *zap.Logger
}

func New(httpClient *http.Client, slackClient *slack.Client, org, teamSlug string, log *zap.Logger) *Cache {
	return &Cache{
		approverIDs: make(map[string]struct{}),
		gh:          github.NewClient(httpClient),
		slack:       slackClient,
		org:         org,
		teamSlug:    teamSlug,
		log:         log,
	}
}

// IsApprover returns true if the Slack user ID is in the cached approver set.
func (c *Cache) IsApprover(slackUserID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.approverIDs[slackUserID]
	return ok
}

// Refresh fetches the current GitHub team roster and rebuilds the Slack user
// ID set. Errors are logged but do not clear the existing cache.
func (c *Cache) Refresh(ctx context.Context) error {
	members, err := c.fetchTeamMembers(ctx)
	if err != nil {
		return fmt.Errorf("fetch team members: %w", err)
	}

	ids := make(map[string]struct{}, len(members))
	for _, login := range members {
		slackID, err := c.resolveSlackID(ctx, login)
		if err != nil {
			// Log and skip — one bad mapping should not block the rest.
			c.log.Warn("approver cache: could not resolve Slack ID",
				zap.String("github_login", login), zap.Error(err))
			continue
		}
		ids[slackID] = struct{}{}
	}

	c.mu.Lock()
	c.approverIDs = ids
	c.mu.Unlock()

	c.log.Info("approver cache refreshed", zap.Int("count", len(ids)))
	return nil
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
