package github

import (
	"context"
	"fmt"
	"regexp"
	"time"

	gh "github.com/google/go-github/v60/github"
)

// DeployCommit holds the parsed fields from a deploy commit message.
type DeployCommit struct {
	App         string
	Tag         string
	SHA         string
	CommittedAt time.Time
}

var deployCommitRe = regexp.MustCompile(`^deploy\((.+)\): update image tag to (.+)$`)

// ListDeployCommits returns up to limit deploy commits that touched path,
// parsed from the conventional commit message format used by the bot.
// Results are ordered newest-first (GitHub's default).
func (c *Client) ListDeployCommits(ctx context.Context, path string, limit int) ([]DeployCommit, error) {
	var results []DeployCommit
	perPage := limit
	if perPage > 100 {
		perPage = 100
	}
	opts := &gh.CommitsListOptions{
		Path:        path,
		ListOptions: gh.ListOptions{PerPage: perPage},
	}
	for {
		commits, resp, err := c.gh.Repositories.ListCommits(ctx, c.org, c.repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list commits for %s: %w", path, err)
		}
		for _, commit := range commits {
			msg := commit.GetCommit().GetMessage()
			m := deployCommitRe.FindStringSubmatch(msg)
			if m == nil {
				continue
			}
			var committedAt time.Time
			if ca := commit.GetCommit().GetCommitter(); ca != nil {
				committedAt = ca.GetDate().Time
			}
			results = append(results, DeployCommit{
				App:         m[1],
				Tag:         m[2],
				SHA:         commit.GetSHA(),
				CommittedAt: committedAt,
			})
			if len(results) >= limit {
				return results, nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return results, nil
}

// PRForCommit returns the number and HTML URL of the first merged PR associated
// with sha, or 0/"" if none is found. The API may return multiple PRs for a
// squash-merged commit; the first result is used.
func (c *Client) PRForCommit(ctx context.Context, sha string) (int, string, error) {
	prs, _, err := c.gh.PullRequests.ListPullRequestsWithCommit(ctx, c.org, c.repo, sha,
		&gh.ListOptions{PerPage: 1})
	if err != nil {
		return 0, "", fmt.Errorf("prs for commit %s: %w", sha, err)
	}
	if len(prs) == 0 {
		return 0, "", nil
	}
	return prs[0].GetNumber(), prs[0].GetHTMLURL(), nil
}
