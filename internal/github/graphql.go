package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// graphqlEndpoint is the GitHub GraphQL v4 endpoint, joined onto the client's
// configured BaseURL. We use the http client that go-github already manages
// (via c.gh.Client()) so auth, the retry/rate-limit transport, and the OTEL
// instrumentation all apply identically to GraphQL traffic.
const graphqlEndpoint = "graphql"

// graphqlError is one entry in the GitHub GraphQL "errors" array.
type graphqlError struct {
	Message string   `json:"message"`
	Type    string   `json:"type,omitempty"`
	Path    []string `json:"path,omitempty"`
}

func (e graphqlError) Error() string {
	if e.Type != "" {
		return fmt.Sprintf("%s: %s", e.Type, e.Message)
	}
	return e.Message
}

// graphqlErrors aggregates multiple errors from one GraphQL response.
type graphqlErrors []graphqlError

func (es graphqlErrors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return "graphql: " + strings.Join(parts, "; ")
}

// graphqlDo POSTs a GraphQL query/mutation and decodes the "data" payload
// into out. It honors the same retry-on-rate-limit wrapper as the REST calls,
// and returns a typed graphqlErrors if GitHub reports any errors[].
func (c *Client) graphqlDo(ctx context.Context, query string, variables map[string]any, out any) error {
	body := map[string]any{"query": query}
	if len(variables) > 0 {
		body["variables"] = variables
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal graphql request: %w", err)
	}

	return c.retryOnRateLimit(ctx, func() error {
		// Resolve relative to whatever BaseURL the underlying client is using
		// (so tests with httptest servers continue to work).
		u, err := c.gh.BaseURL.Parse(graphqlEndpoint)
		if err != nil {
			return fmt.Errorf("resolve graphql url: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("build graphql request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := c.gh.Client().Do(req)
		if err != nil {
			return fmt.Errorf("graphql request: %w", err)
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("read graphql response: %w", err)
		}
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("graphql http %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}

		var envelope struct {
			Data   json.RawMessage `json:"data"`
			Errors graphqlErrors   `json:"errors"`
		}
		if err := json.Unmarshal(respBody, &envelope); err != nil {
			return fmt.Errorf("decode graphql response: %w", err)
		}
		if len(envelope.Errors) > 0 {
			return envelope.Errors
		}
		if out != nil && len(envelope.Data) > 0 {
			if err := json.Unmarshal(envelope.Data, out); err != nil {
				return fmt.Errorf("decode graphql data: %w", err)
			}
		}
		return nil
	})
}

// deployState is the data we need to start a deploy: the base branch HEAD
// SHA, the repository's node ID (required by createRef/createPullRequest),
// the current text of the kustomization file, and — for the rebase path —
// the deploy branch's ref node ID if it already exists.
type deployState struct {
	BaseSHA      string
	RepoID       string
	DeployRefID  string // empty if the deploy branch does not yet exist
	FileContents string
	FileExists   bool
}

// fetchDeployState issues a single GraphQL query that fetches every piece of
// state CreateDeployPR / RebaseDeployBranch need to compute their next
// mutation. It collapses what used to be GetRef + GetContents (+ a probe for
// the deploy branch in the rebase path) into one network call.
func (c *Client) fetchDeployState(ctx context.Context, baseBranch, deployBranch, kustomizePath string) (*deployState, error) {
	const query = `query DeployState($owner: String!, $repo: String!, $baseRef: String!, $deployRef: String!, $fileExpr: String!) {
  repository(owner: $owner, name: $repo) {
    id
    baseRef: ref(qualifiedName: $baseRef) { target { oid } }
    deployRef: ref(qualifiedName: $deployRef) { id }
    file: object(expression: $fileExpr) {
      ... on Blob { text }
    }
  }
}`
	vars := map[string]any{
		"owner":     c.org,
		"repo":      c.repo,
		"baseRef":   "refs/heads/" + baseBranch,
		"deployRef": "refs/heads/" + deployBranch,
		"fileExpr":  baseBranch + ":" + kustomizePath,
	}

	var resp struct {
		Repository struct {
			ID      string `json:"id"`
			BaseRef *struct {
				Target struct {
					OID string `json:"oid"`
				} `json:"target"`
			} `json:"baseRef"`
			DeployRef *struct {
				ID string `json:"id"`
			} `json:"deployRef"`
			File *struct {
				Text string `json:"text"`
			} `json:"file"`
		} `json:"repository"`
	}
	if err := c.graphqlDo(ctx, query, vars, &resp); err != nil {
		return nil, err
	}
	if resp.Repository.BaseRef == nil {
		return nil, fmt.Errorf("base branch %q not found", baseBranch)
	}
	st := &deployState{
		BaseSHA: resp.Repository.BaseRef.Target.OID,
		RepoID:  resp.Repository.ID,
	}
	if resp.Repository.DeployRef != nil {
		st.DeployRefID = resp.Repository.DeployRef.ID
	}
	if resp.Repository.File != nil {
		st.FileContents = resp.Repository.File.Text
		st.FileExists = true
	}
	return st, nil
}

// createDeployCommitAndPR runs createRef + createCommitOnBranch +
// createPullRequest as a single GraphQL mutation. The three operations
// execute sequentially server-side, so the branch created in m1 is visible
// to m2 by name, and the head branch named in m3 is the one m1 just made.
//
// On success returns the PR number and HTML URL.
func (c *Client) createDeployCommitAndPR(
	ctx context.Context,
	repoID, baseBranch, deployBranch, baseSHA, kustomizePath, fileContents, commitMsg, prTitle, prBody string,
) (int, string, error) {
	const mutation = `mutation DeployCommit(
  $repoId: ID!,
  $deployRef: String!,
  $baseSHA: GitObjectID!,
  $branchName: String!,
  $headline: String!,
  $additions: [FileAddition!]!,
  $prTitle: String!,
  $prBody: String!,
  $baseBranch: String!,
  $headBranch: String!
) {
  createRef(input: {repositoryId: $repoId, name: $deployRef, oid: $baseSHA}) {
    ref { name }
  }
  createCommitOnBranch(input: {
    branch: {repositoryNameWithOwner: $branchName, branchName: $headBranch},
    message: {headline: $headline},
    fileChanges: {additions: $additions},
    expectedHeadOid: $baseSHA
  }) {
    commit { oid }
  }
  createPullRequest(input: {
    repositoryId: $repoId,
    baseRefName: $baseBranch,
    headRefName: $headBranch,
    title: $prTitle,
    body: $prBody
  }) {
    pullRequest { number url }
  }
}`

	vars := map[string]any{
		"repoId":     repoID,
		"deployRef":  "refs/heads/" + deployBranch,
		"baseSHA":    baseSHA,
		"branchName": c.org + "/" + c.repo,
		"headline":   commitMsg,
		"additions": []map[string]string{{
			"path":     kustomizePath,
			"contents": base64Std(fileContents),
		}},
		"prTitle":    prTitle,
		"prBody":     prBody,
		"baseBranch": baseBranch,
		"headBranch": deployBranch,
	}

	var resp struct {
		CreatePullRequest struct {
			PullRequest struct {
				Number int    `json:"number"`
				URL    string `json:"url"`
			} `json:"pullRequest"`
		} `json:"createPullRequest"`
	}
	if err := c.graphqlDo(ctx, mutation, vars, &resp); err != nil {
		return 0, "", err
	}
	if resp.CreatePullRequest.PullRequest.Number == 0 {
		return 0, "", fmt.Errorf("createPullRequest returned no PR")
	}
	return resp.CreatePullRequest.PullRequest.Number, resp.CreatePullRequest.PullRequest.URL, nil
}

// rebaseAndCommit fast-forwards the deploy branch to baseSHA via updateRef
// (force) and then adds a single commit with the new file contents on top.
// Both operations run as one GraphQL mutation, replacing the seven REST
// calls the old Git Data API path required.
func (c *Client) rebaseAndCommit(
	ctx context.Context,
	deployRefID, deployBranch, baseSHA, kustomizePath, fileContents, commitMsg string,
) error {
	if deployRefID == "" {
		return fmt.Errorf("rebase: deploy branch ref id is empty")
	}
	const mutation = `mutation Rebase(
  $refId: ID!,
  $baseSHA: GitObjectID!,
  $branchName: String!,
  $headBranch: String!,
  $headline: String!,
  $additions: [FileAddition!]!
) {
  updateRef(input: {refId: $refId, oid: $baseSHA, force: true}) {
    ref { name }
  }
  createCommitOnBranch(input: {
    branch: {repositoryNameWithOwner: $branchName, branchName: $headBranch},
    message: {headline: $headline},
    fileChanges: {additions: $additions},
    expectedHeadOid: $baseSHA
  }) {
    commit { oid }
  }
}`

	vars := map[string]any{
		"refId":      deployRefID,
		"baseSHA":    baseSHA,
		"branchName": c.org + "/" + c.repo,
		"headBranch": deployBranch,
		"headline":   commitMsg,
		"additions": []map[string]string{{
			"path":     kustomizePath,
			"contents": base64Std(fileContents),
		}},
	}
	return c.graphqlDo(ctx, mutation, vars, nil)
}
