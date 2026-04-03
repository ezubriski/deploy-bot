package github

import (
	"context"
	"fmt"

	gh "github.com/google/go-github/v60/github"

	"github.com/ezubriski/deploy-bot/internal/sanitize"
)

// AddComment posts a comment on the given PR.
func (c *Client) AddComment(ctx context.Context, prNumber int, body string) error {
	return c.retryOnRateLimit(ctx, func() error {
		_, _, err := c.gh.Issues.CreateComment(ctx, c.org, c.repo, prNumber, &gh.IssueComment{
			Body: gh.String(body),
		})
		if err != nil {
			return fmt.Errorf("create comment: %w", err)
		}
		return nil
	})
}

// CommentRequested posts a "deployment requested" comment.
func (c *Client) CommentRequested(ctx context.Context, prNumber int, requester, app, tag, reason string) error {
	body := fmt.Sprintf(
		"**Deployment requested** by @%s\n\n- **App:** %s\n- **Tag:** `%s`\n- **Reason:** %s",
		requester, app, tag, sanitize.GitHubMarkdown(reason),
	)
	return c.AddComment(ctx, prNumber, body)
}

// CommentApproved posts an "approved" comment.
func (c *Client) CommentApproved(ctx context.Context, prNumber int, approver string) error {
	body := fmt.Sprintf("**Approved** by @%s — merging now.", approver)
	return c.AddComment(ctx, prNumber, body)
}

// CommentRejected posts a "rejected" comment.
func (c *Client) CommentRejected(ctx context.Context, prNumber int, approver, reason string) error {
	body := fmt.Sprintf("**Rejected** by @%s\n\n**Reason:** %s", approver, sanitize.GitHubMarkdown(reason))
	return c.AddComment(ctx, prNumber, body)
}

// CommentExpired posts an "expired" comment.
func (c *Client) CommentExpired(ctx context.Context, prNumber int, staleDuration string) error {
	body := fmt.Sprintf("**Deployment expired** — no approval received within %s. Closing PR.", staleDuration)
	return c.AddComment(ctx, prNumber, body)
}

// CommentCancelled posts a "cancelled" comment.
func (c *Client) CommentCancelled(ctx context.Context, prNumber int, requester string) error {
	body := fmt.Sprintf("**Cancelled** by @%s.", requester)
	return c.AddComment(ctx, prNumber, body)
}
