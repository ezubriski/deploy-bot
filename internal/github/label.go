package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	gh "github.com/google/go-github/v60/github"
)

const LabelColor = "0075ca"

// PRMeta is embedded as an HTML comment in every deploy PR body so the bot
// can reconstruct state after a Redis flush.
type PRMeta struct {
	RequesterSlackID string `json:"requester_id"`
	App              string `json:"app"`
	Tag              string `json:"tag"`
}

var metaRegex = regexp.MustCompile(`<!-- deploy-bot-meta: ({.*?}) -->`)

// ParsePRMeta extracts deploy-bot metadata from a PR body.
// Returns false if the body contains no metadata (e.g. old-format PRs).
func ParsePRMeta(body string) (*PRMeta, bool) {
	m := metaRegex.FindStringSubmatch(body)
	if m == nil {
		return nil, false
	}
	var meta PRMeta
	if err := json.Unmarshal([]byte(m[1]), &meta); err != nil {
		return nil, false
	}
	return &meta, true
}

// EnsureLabel creates the label in the repo if it does not already exist.
func (c *Client) EnsureLabel(ctx context.Context, name, color string) error {
	var getResp *gh.Response
	getErr := c.retryOnRateLimit(ctx, func() error {
		var err error
		_, getResp, err = c.gh.Issues.GetLabel(ctx, c.org, c.repo, name)
		return err
	})
	if getErr == nil {
		return nil // already exists
	}
	if getResp == nil || getResp.StatusCode != 404 {
		return fmt.Errorf("get label: %w", getErr)
	}
	return c.retryOnRateLimit(ctx, func() error {
		_, _, err := c.gh.Issues.CreateLabel(ctx, c.org, c.repo, &gh.Label{
			Name:  gh.String(name),
			Color: gh.String(color),
		})
		if err != nil {
			return fmt.Errorf("create label: %w", err)
		}
		return nil
	})
}

// AddLabels applies one or more labels to a PR (PRs are issues in the GitHub API).
func (c *Client) AddLabels(ctx context.Context, prNumber int, labels []string) error {
	return c.retryOnRateLimit(ctx, func() error {
		_, _, err := c.gh.Issues.AddLabelsToIssue(ctx, c.org, c.repo, prNumber, labels)
		if err != nil {
			return fmt.Errorf("add labels to PR %d: %w", prNumber, err)
		}
		return nil
	})
}

// RemoveLabel removes a label from a PR. Returns nil if the label was not present.
func (c *Client) RemoveLabel(ctx context.Context, prNumber int, label string) error {
	var rmResp *gh.Response
	if err := c.retryOnRateLimit(ctx, func() error {
		var err error
		rmResp, err = c.gh.Issues.RemoveLabelForIssue(ctx, c.org, c.repo, prNumber, label)
		return err
	}); err != nil {
		if rmResp != nil && rmResp.StatusCode == 404 {
			return nil // already absent
		}
		return fmt.Errorf("remove label from PR %d: %w", prNumber, err)
	}
	return nil
}

// ListOpenPRsWithLabel returns all open pull requests carrying the given label.
func (c *Client) ListOpenPRsWithLabel(ctx context.Context, label string) ([]*gh.Issue, error) {
	var all []*gh.Issue
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{label},
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for {
		var issues []*gh.Issue
		var resp *gh.Response
		if err := c.retryOnRateLimit(ctx, func() error {
			var err error
			issues, resp, err = c.gh.Issues.ListByRepo(ctx, c.org, c.repo, opts)
			return err
		}); err != nil {
			return nil, fmt.Errorf("list issues: %w", err)
		}
		for _, issue := range issues {
			if issue.PullRequestLinks != nil {
				all = append(all, issue)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}
