package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	gh "github.com/google/go-github/v60/github"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/sanitize"
)

// ErrCINotPassed is returned by MergePR when GitHub blocks the merge because a
// required status check has not passed. The PR cannot be merged until CI
// succeeds; callers should notify the approver and leave the PR open.
var ErrCINotPassed = errors.New("required status check has not passed")

// ErrDraftPR is returned by MergePR when the pull request is still in draft
// state. The author must mark it ready for review before it can be merged.
var ErrDraftPR = errors.New("pull request is in draft state")

// ErrHeadModified is returned by MergePR when GitHub returns HTTP 409,
// indicating the head branch was modified between when we checked mergeability
// and when we attempted the merge. Retrying the merge directly (without
// rebasing) is usually sufficient.
var ErrHeadModified = errors.New("head branch was modified; retry the merge")

// ErrNoChange is returned by CreateDeployPR and RebaseDeployBranch when the
// requested tag is already the current value in the kustomization file. No PR
// is created and no branch is left behind.
var ErrNoChange = errors.New("tag is already current; no changes to deploy")

// ErrMergeConflict is returned by MergePR when GitHub reports the pull request
// is not mergeable (HTTP 405). The caller should attempt RebaseDeployBranch
// and retry.
var ErrMergeConflict = errors.New("pull request is not mergeable")

type Client struct {
	gh    *gh.Client
	org   string
	repo  string
	log   *zap.Logger
	retry RetryConfig
}

func NewClient(httpClient *http.Client, org, repo string, log *zap.Logger, retry RetryConfig) *Client {
	if log == nil {
		log = zap.NewNop()
	}
	if retry.MaxRetries == 0 {
		retry = defaultRetryConfig()
	}
	return &Client{
		gh:    gh.NewClient(httpClient),
		org:   org,
		repo:  repo,
		log:   log,
		retry: retry,
	}
}

// CreateDeployPR creates a branch, commits the kustomize image tag update, and
// opens a PR. Returns ErrNoChange if the tag is already current in the file.
func (c *Client) CreateDeployPR(ctx context.Context, params CreatePRParams) (int, string, error) {
	if !sanitize.TagIsSafe(params.Tag) {
		return 0, "", fmt.Errorf("tag %q contains unsafe characters", params.Tag)
	}
	branch := fmt.Sprintf("deploy/%s-%s-%s", params.Environment, params.App, sanitize.BranchName(params.Tag))

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

	// No-op: tag is already current. Delete the branch we just created and
	// signal the caller so they can notify without creating a PR.
	if updated == content {
		_ = c.retryOnRateLimit(ctx, func() error {
			_, err := c.gh.Git.DeleteRef(ctx, c.org, c.repo, "refs/heads/"+branch)
			return err
		})
		return 0, "", ErrNoChange
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
		"**Environment:** %s\n**App:** %s\n**Tag:** `%s`\n**Requester:** %s\n**Reason:** %s\n\n<!-- deploy-bot-meta: %s -->",
		params.Environment, params.App, params.Tag, formatRequester(params.Requester), sanitize.GitHubMarkdown(params.Reason), string(metaJSON),
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
			_ = err
		}
	}

	return pr.GetNumber(), pr.GetHTMLURL(), nil
}

// MergePR merges a pull request using the configured merge method. Returns
// ErrMergeConflict if GitHub reports the PR is not mergeable (HTTP 405).
func (c *Client) MergePR(ctx context.Context, prNumber int, mergeMethod string) error {
	return c.retryOnRateLimit(ctx, func() error {
		_, _, err := c.gh.PullRequests.Merge(ctx, c.org, c.repo, prNumber, "", &gh.PullRequestOptions{
			MergeMethod: mergeMethod,
		})
		if err != nil {
			var ghErr *gh.ErrorResponse
			if errors.As(err, &ghErr) && ghErr.Response != nil {
				switch ghErr.Response.StatusCode {
				case 405:
					msg := strings.ToLower(ghErr.Message)
					switch {
					case strings.Contains(msg, "status check") || strings.Contains(msg, "required"):
						return ErrCINotPassed
					case strings.Contains(msg, "draft"):
						return ErrDraftPR
					default: // conflict or branch out of date
						return ErrMergeConflict
					}
				case 409:
					return ErrHeadModified
				}
			}
			return fmt.Errorf("merge PR: %w", err)
		}
		return nil
	})
}

// RebaseDeployBranch re-applies params.Tag onto the current HEAD of
// params.BaseBranch and force-updates the deploy branch in place using the
// Git Data API. Call this after MergePR returns ErrMergeConflict, then retry
// MergePR. Returns ErrNoChange if the tag is already current on the base
// branch (the deploy happened through another means).
func (c *Client) RebaseDeployBranch(ctx context.Context, params CreatePRParams) error {
	branch := fmt.Sprintf("deploy/%s-%s-%s", params.Environment, params.App, sanitize.BranchName(params.Tag))

	// Step 1: get current default branch HEAD SHA.
	var baseRef *gh.Reference
	if err := c.retryOnRateLimit(ctx, func() error {
		var err error
		baseRef, _, err = c.gh.Git.GetRef(ctx, c.org, c.repo, "refs/heads/"+params.BaseBranch)
		return err
	}); err != nil {
		return fmt.Errorf("rebase: get base ref: %w", err)
	}
	headSHA := baseRef.Object.GetSHA()

	// Step 2: fetch the kustomization file at the current base branch HEAD.
	var fileContent *gh.RepositoryContent
	if err := c.retryOnRateLimit(ctx, func() error {
		var err error
		fileContent, _, _, err = c.gh.Repositories.GetContents(ctx, c.org, c.repo, params.KustomizePath, &gh.RepositoryContentGetOptions{
			Ref: params.BaseBranch,
		})
		return err
	}); err != nil {
		return fmt.Errorf("rebase: get kustomization file: %w", err)
	}

	content, err := fileContent.GetContent()
	if err != nil {
		return fmt.Errorf("rebase: decode kustomization file: %w", err)
	}

	// Step 3: apply the tag update.
	updated, err := updateNewTag(content, params.Tag)
	if err != nil {
		return fmt.Errorf("rebase: update kustomization tag: %w", err)
	}
	if updated == content {
		// Tag is already on the base branch — the deploy already happened.
		return ErrNoChange
	}

	// Step 4: create a new blob with the updated content.
	var blob *gh.Blob
	if err := c.retryOnRateLimit(ctx, func() error {
		var err error
		blob, _, err = c.gh.Git.CreateBlob(ctx, c.org, c.repo, &gh.Blob{
			Content:  gh.String(updated),
			Encoding: gh.String("utf-8"),
		})
		return err
	}); err != nil {
		return fmt.Errorf("rebase: create blob: %w", err)
	}

	// Step 5: get the tree SHA from the current HEAD commit.
	var headCommit *gh.Commit
	if err := c.retryOnRateLimit(ctx, func() error {
		var err error
		headCommit, _, err = c.gh.Git.GetCommit(ctx, c.org, c.repo, headSHA)
		return err
	}); err != nil {
		return fmt.Errorf("rebase: get head commit: %w", err)
	}

	// Step 6: create a new tree with the updated blob substituted in.
	var tree *gh.Tree
	if err := c.retryOnRateLimit(ctx, func() error {
		var err error
		tree, _, err = c.gh.Git.CreateTree(ctx, c.org, c.repo, headCommit.Tree.GetSHA(), []*gh.TreeEntry{
			{
				Path: gh.String(params.KustomizePath),
				Mode: gh.String("100644"),
				Type: gh.String("blob"),
				SHA:  gh.String(blob.GetSHA()),
			},
		})
		return err
	}); err != nil {
		return fmt.Errorf("rebase: create tree: %w", err)
	}

	// Step 7: create a commit whose parent is the current HEAD.
	commitMsg := fmt.Sprintf("deploy(%s/%s): update image tag to %s", params.Environment, params.App, params.Tag)
	var newCommit *gh.Commit
	if err := c.retryOnRateLimit(ctx, func() error {
		var err error
		newCommit, _, err = c.gh.Git.CreateCommit(ctx, c.org, c.repo, &gh.Commit{
			Message: gh.String(commitMsg),
			Tree:    &gh.Tree{SHA: gh.String(tree.GetSHA())},
			Parents: []*gh.Commit{{SHA: gh.String(headSHA)}},
		}, nil)
		return err
	}); err != nil {
		return fmt.Errorf("rebase: create commit: %w", err)
	}

	// Step 8: force-update the deploy branch ref to the new commit.
	if err := c.retryOnRateLimit(ctx, func() error {
		_, _, err := c.gh.Git.UpdateRef(ctx, c.org, c.repo, &gh.Reference{
			Ref:    gh.String("refs/heads/" + branch),
			Object: &gh.GitObject{SHA: gh.String(newCommit.GetSHA())},
		}, true)
		return err
	}); err != nil {
		return fmt.Errorf("rebase: update branch ref: %w", err)
	}

	c.log.Info("rebased deploy branch",
		zap.String("branch", branch),
		zap.String("base_sha", headSHA),
		zap.String("new_commit", newCommit.GetSHA()),
	)
	return nil
}

// ClosePR closes a pull request without merging. Returns nil if the PR is
// already closed or does not exist (422/404 — goal already achieved).
func (c *Client) ClosePR(ctx context.Context, prNumber int) error {
	state := "closed"
	return c.retryOnRateLimit(ctx, func() error {
		_, resp, err := c.gh.PullRequests.Edit(ctx, c.org, c.repo, prNumber, &gh.PullRequest{
			State: &state,
		})
		if err != nil {
			if resp != nil && (resp.StatusCode == 404 || resp.StatusCode == 422) {
				return nil // already closed or not found
			}
			return fmt.Errorf("close PR: %w", err)
		}
		return nil
	})
}

// DeleteBranch deletes a git branch from the repository. Returns nil if the
// branch does not exist (422 "Reference does not exist" — goal already achieved).
func (c *Client) DeleteBranch(ctx context.Context, branch string) error {
	return c.retryOnRateLimit(ctx, func() error {
		resp, err := c.gh.Git.DeleteRef(ctx, c.org, c.repo, "refs/heads/"+branch)
		if err != nil {
			if resp != nil && resp.StatusCode == 422 {
				return nil // branch already gone
			}
			return fmt.Errorf("delete branch %s: %w", branch, err)
		}
		return nil
	})
}

// GetFileContent returns the decoded text content of a file at the given ref
// (branch name, tag, or commit SHA). Useful for inspecting current state
// without making any changes.
func (c *Client) GetFileContent(ctx context.Context, path, ref string) (string, error) {
	var fc *gh.RepositoryContent
	if err := c.retryOnRateLimit(ctx, func() error {
		var err error
		fc, _, _, err = c.gh.Repositories.GetContents(ctx, c.org, c.repo, path, &gh.RepositoryContentGetOptions{Ref: ref})
		return err
	}); err != nil {
		return "", fmt.Errorf("get file content: %w", err)
	}
	content, err := fc.GetContent()
	if err != nil {
		return "", fmt.Errorf("decode file content: %w", err)
	}
	return content, nil
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
