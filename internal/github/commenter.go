package github

import (
	"context"
	"fmt"

	gh "github.com/google/go-github/v60/github"

	"github.com/ezubriski/deploy-bot/internal/sanitize"
)

const resubmitHint = "To redeploy, run `/deploy` or `@bot deploy <app> <tag>` in Slack."

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
		"**Deployment requested** by %s\n\n- **App:** %s\n- **Tag:** `%s`\n- **Reason:** %s",
		formatRequester(requester), app, tag, sanitize.GitHubMarkdown(reason),
	)
	return c.AddComment(ctx, prNumber, body)
}

// CommentApproved posts an "approved" comment.
func (c *Client) CommentApproved(ctx context.Context, prNumber int, approver string) error {
	body := fmt.Sprintf("**Approved** by %s — merging now.", formatRequester(approver))
	return c.AddComment(ctx, prNumber, body)
}

// CommentRejected posts a "rejected" comment on a PR that is being closed.
func (c *Client) CommentRejected(ctx context.Context, prNumber int, approver, reason string) error {
	body := fmt.Sprintf(
		"**Rejected** by %s\n\n**Reason:** %s\n\n---\n%s",
		formatRequester(approver), sanitize.GitHubMarkdown(reason), resubmitHint,
	)
	return c.AddComment(ctx, prNumber, body)
}

// CommentExpired posts an "expired" comment on a PR that is being closed.
func (c *Client) CommentExpired(ctx context.Context, prNumber int, staleDuration string) error {
	body := fmt.Sprintf(
		"**Deployment expired** — no approval received within %s. Closing PR.\n\n---\n%s",
		staleDuration, resubmitHint,
	)
	return c.AddComment(ctx, prNumber, body)
}

// CommentCancelled posts a "cancelled" comment on a PR that is being closed.
func (c *Client) CommentCancelled(ctx context.Context, prNumber int, requester string) error {
	body := fmt.Sprintf(
		"**Cancelled** by %s.\n\n---\n%s",
		formatRequester(requester), resubmitHint,
	)
	return c.AddComment(ctx, prNumber, body)
}

// CommentNoOp posts a comment on a PR that is being closed because the
// target tag is already deployed.
func (c *Client) CommentNoOp(ctx context.Context, prNumber int, app, tag string) error {
	body := fmt.Sprintf(
		"**No-op** — `%s` is already running `%s`. No changes to deploy. Closing PR.\n\n---\nTo deploy a different tag, run `/deploy %s` or `@bot deploy %s <tag>` in Slack.",
		app, tag, app, app,
	)
	return c.AddComment(ctx, prNumber, body)
}

// CommentAutoDeployFailed posts a comment on a PR that is being closed because
// an ECR auto-deploy merge failed.
func (c *Client) CommentAutoDeployFailed(ctx context.Context, prNumber int, reason error) error {
	body := fmt.Sprintf(
		"**Auto-deploy failed** — %v. Closing PR.\n\n---\n%s",
		reason, resubmitHint,
	)
	return c.AddComment(ctx, prNumber, body)
}

// formatRequester formats a requester/approver name for GitHub comments.
// Human users get an @mention; the ECR sentinel gets a descriptive label.
func formatRequester(name string) string {
	if name == "ECR" {
		return "**deploy-bot** (ECR push auto-deploy)"
	}
	return "@" + name
}
