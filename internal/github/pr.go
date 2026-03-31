package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	gh "github.com/google/go-github/v60/github"
	"golang.org/x/oauth2"
)

type Client struct {
	gh   *gh.Client
	org  string
	repo string
}

func NewClient(token, org, repo string) *Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	httpClient := oauth2.NewClient(context.Background(), ts)
	return &Client{
		gh:   gh.NewClient(httpClient),
		org:  org,
		repo: repo,
	}
}

// CreateDeployPR creates a branch, commits the kustomize image tag update, and opens a PR.
func (c *Client) CreateDeployPR(ctx context.Context, params CreatePRParams) (int, string, error) {
	branch := fmt.Sprintf("deploy/%s-%s", params.App, sanitizeBranchName(params.Tag))

	// Get default branch SHA
	ref, _, err := c.gh.Git.GetRef(ctx, c.org, c.repo, "refs/heads/"+params.BaseBranch)
	if err != nil {
		return 0, "", fmt.Errorf("get base branch ref: %w", err)
	}
	baseSHA := ref.Object.GetSHA()

	// Create new branch
	_, _, err = c.gh.Git.CreateRef(ctx, c.org, c.repo, &gh.Reference{
		Ref:    gh.String("refs/heads/" + branch),
		Object: &gh.GitObject{SHA: gh.String(baseSHA)},
	})
	if err != nil {
		return 0, "", fmt.Errorf("create branch: %w", err)
	}

	// Get current file
	fileContent, _, _, err := c.gh.Repositories.GetContents(ctx, c.org, c.repo, params.KustomizePath, &gh.RepositoryContentGetOptions{
		Ref: branch,
	})
	if err != nil {
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

	commitMsg := fmt.Sprintf("deploy(%s): update image tag to %s", params.App, params.Tag)
	_, _, err = c.gh.Repositories.UpdateFile(ctx, c.org, c.repo, params.KustomizePath, &gh.RepositoryContentFileOptions{
		Message: gh.String(commitMsg),
		Content: []byte(updated),
		SHA:     gh.String(fileContent.GetSHA()),
		Branch:  gh.String(branch),
	})
	if err != nil {
		return 0, "", fmt.Errorf("update kustomization file: %w", err)
	}

	// Embed recovery metadata as an HTML comment (not rendered by GitHub).
	metaJSON, _ := json.Marshal(PRMeta{
		RequesterSlackID: params.RequesterSlackID,
		App:              params.App,
		Tag:              params.Tag,
	})
	prBody := fmt.Sprintf(
		"**App:** %s\n**Tag:** %s\n**Requester:** @%s\n**Reason:** %s\n\n<!-- deploy-bot-meta: %s -->",
		params.App, params.Tag, params.Requester, params.Reason, string(metaJSON),
	)
	pr, _, err := c.gh.PullRequests.Create(ctx, c.org, c.repo, &gh.NewPullRequest{
		Title: gh.String(fmt.Sprintf("deploy(%s): %s", params.App, params.Tag)),
		Head:  gh.String(branch),
		Base:  gh.String(params.BaseBranch),
		Body:  gh.String(prBody),
	})
	if err != nil {
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
	_, _, err := c.gh.PullRequests.Merge(ctx, c.org, c.repo, prNumber, "", &gh.PullRequestOptions{
		MergeMethod: mergeMethod,
	})
	if err != nil {
		return fmt.Errorf("merge PR: %w", err)
	}
	return nil
}

// ClosePR closes a pull request without merging.
func (c *Client) ClosePR(ctx context.Context, prNumber int) error {
	state := "closed"
	_, _, err := c.gh.PullRequests.Edit(ctx, c.org, c.repo, prNumber, &gh.PullRequest{
		State: &state,
	})
	if err != nil {
		return fmt.Errorf("close PR: %w", err)
	}
	return nil
}

// GetDefaultBranch returns the default branch name for the repo.
func (c *Client) GetDefaultBranch(ctx context.Context) (string, error) {
	repo, _, err := c.gh.Repositories.Get(ctx, c.org, c.repo)
	if err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	return repo.GetDefaultBranch(), nil
}

type CreatePRParams struct {
	App              string
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
