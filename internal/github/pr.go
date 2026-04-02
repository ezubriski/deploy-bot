package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	gh "github.com/google/go-github/v60/github"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

type Client struct {
	gh    *gh.Client
	org   string
	repo  string
	log   *zap.Logger
	retry RetryConfig
}

func NewClient(token, org, repo string, log *zap.Logger, retry RetryConfig) *Client {
	if log == nil {
		log = zap.NewNop()
	}
	if retry.MaxRetries == 0 {
		retry = defaultRetryConfig()
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	httpClient := oauth2.NewClient(context.Background(), ts)
	return &Client{
		gh:    gh.NewClient(httpClient),
		org:   org,
		repo:  repo,
		log:   log,
		retry: retry,
	}
}

// CreateDeployPR creates a branch, commits the kustomize image tag update, and opens a PR.
func (c *Client) CreateDeployPR(ctx context.Context, params CreatePRParams) (int, string, error) {
	branch := fmt.Sprintf("deploy/%s-%s-%s", params.Environment, params.App, sanitizeBranchName(params.Tag))

	// Get default branch SHA
	var ref *gh.Reference
	if err := c.retryOnRateLimit(ctx, func() error {
		var err error
		ref, _, err = c.gh.Git.GetRef(ctx, c.org, c.repo, "refs/heads/"+params.BaseBranch)
		return err
	}); err != nil {
		return 0, "", fmt.Errorf("get base branch ref: %w", err)
	}
	baseSHA := ref.Object.GetSHA()

	// Create new branch
	if err := c.retryOnRateLimit(ctx, func() error {
		_, _, err := c.gh.Git.CreateRef(ctx, c.org, c.repo, &gh.Reference{
			Ref:    gh.String("refs/heads/" + branch),
			Object: &gh.GitObject{SHA: gh.String(baseSHA)},
		})
		return err
	}); err != nil {
		return 0, "", fmt.Errorf("create branch: %w", err)
	}

	// Get current file
	var fileContent *gh.RepositoryContent
	if err := c.retryOnRateLimit(ctx, func() error {
		var err error
		fileContent, _, _, err = c.gh.Repositories.GetContents(ctx, c.org, c.repo, params.KustomizePath, &gh.RepositoryContentGetOptions{
			Ref: branch,
		})
		return err
	}); err != nil {
		return 0, "", fmt.Errorf("get kustomization file: %w", err)
	}

	content, err := fileContent.GetContent()
	if err != nil {
		return 0, "", fmt.Errorf("decode kustomization file: %w", err)
	}

	// Update newTag value
	updated, err := updateNewTag(content, params.Tag)
	if err != nil {
		return 0, "", fmt.Errorf("update kustomization tag: %w", err)
	}

	commitMsg := fmt.Sprintf("deploy(%s/%s): update image tag to %s", params.Environment, params.App, params.Tag)
	if err := c.retryOnRateLimit(ctx, func() error {
		_, _, err := c.gh.Repositories.UpdateFile(ctx, c.org, c.repo, params.KustomizePath, &gh.RepositoryContentFileOptions{
			Message: gh.String(commitMsg),
			Content: []byte(updated),
			SHA:     gh.String(fileContent.GetSHA()),
			Branch:  gh.String(branch),
		})
		return err
	}); err != nil {
		return 0, "", fmt.Errorf("update kustomization file: %w", err)
	}

	// Embed recovery metadata as an HTML comment (not rendered by GitHub).
	metaJSON, _ := json.Marshal(PRMeta{
		RequesterSlackID: params.RequesterSlackID,
		App:              params.App,
		Environment:      params.Environment,
		Tag:              params.Tag,
	})
	prBody := fmt.Sprintf(
		"**Environment:** %s\n**App:** %s\n**Tag:** %s\n**Requester:** @%s\n**Reason:** %s\n\n<!-- deploy-bot-meta: %s -->",
		params.Environment, params.App, params.Tag, params.Requester, params.Reason, string(metaJSON),
	)

	var pr *gh.PullRequest
	if err := c.retryOnRateLimit(ctx, func() error {
		var err error
		pr, _, err = c.gh.PullRequests.Create(ctx, c.org, c.repo, &gh.NewPullRequest{
			Title: gh.String(fmt.Sprintf("deploy(%s/%s): %s", params.Environment, params.App, params.Tag)),
			Head:  gh.String(branch),
			Base:  gh.String(params.BaseBranch),
			Body:  gh.String(prBody),
		})
		return err
	}); err != nil {
		return 0, "", fmt.Errorf("create PR: %w", err)
	}

	if len(params.Labels) > 0 {
		if err := c.AddLabels(ctx, pr.GetNumber(), params.Labels); err != nil {
			// Non-fatal: log at call site if needed, deploy continues.
			_ = err
		}
	}

	return pr.GetNumber(), pr.GetHTMLURL(), nil
}

// MergePR merges a pull request using the configured merge method.
func (c *Client) MergePR(ctx context.Context, prNumber int, mergeMethod string) error {
	return c.retryOnRateLimit(ctx, func() error {
		_, _, err := c.gh.PullRequests.Merge(ctx, c.org, c.repo, prNumber, "", &gh.PullRequestOptions{
			MergeMethod: mergeMethod,
		})
		if err != nil {
			return fmt.Errorf("merge PR: %w", err)
		}
		return nil
	})
}

// ClosePR closes a pull request without merging.
func (c *Client) ClosePR(ctx context.Context, prNumber int) error {
	state := "closed"
	return c.retryOnRateLimit(ctx, func() error {
		_, _, err := c.gh.PullRequests.Edit(ctx, c.org, c.repo, prNumber, &gh.PullRequest{
			State: &state,
		})
		if err != nil {
			return fmt.Errorf("close PR: %w", err)
		}
		return nil
	})
}

// DeleteBranch deletes a git branch from the repository.
func (c *Client) DeleteBranch(ctx context.Context, branch string) error {
	return c.retryOnRateLimit(ctx, func() error {
		_, err := c.gh.Git.DeleteRef(ctx, c.org, c.repo, "refs/heads/"+branch)
		if err != nil {
			return fmt.Errorf("delete branch %s: %w", branch, err)
		}
		return nil
	})
}

// GetDefaultBranch returns the default branch name for the repo.
func (c *Client) GetDefaultBranch(ctx context.Context) (string, error) {
	var repo *gh.Repository
	if err := c.retryOnRateLimit(ctx, func() error {
		var err error
		repo, _, err = c.gh.Repositories.Get(ctx, c.org, c.repo)
		return err
	}); err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	return repo.GetDefaultBranch(), nil
}

type CreatePRParams struct {
	App              string
	Environment      string
	Tag              string
	KustomizePath    string
	BaseBranch       string
	Requester        string
	Reason           string
	RequesterSlackID string
	Labels           []string
}

var newTagRegex = regexp.MustCompile(`(newTag:\s*)(\S+)`)

// updateNewTag replaces the newTag value in a kustomization.yaml content string.
func updateNewTag(content, newTag string) (string, error) {
	if !newTagRegex.MatchString(content) {
		return "", fmt.Errorf("newTag not found in kustomization")
	}
	return newTagRegex.ReplaceAllString(content, "${1}"+newTag), nil
}

// sanitizeBranchName makes a tag safe for use in a git branch name.
func sanitizeBranchName(tag string) string {
	r := strings.NewReplacer("/", "-", ":", "-", "+", "-", " ", "-")
	return r.Replace(tag)
}
