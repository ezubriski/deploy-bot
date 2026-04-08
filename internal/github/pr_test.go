package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- updateNewTag ---

func TestUpdateNewTag(t *testing.T) {
	cases := []struct {
		name    string
		content string
		newTag  string
		want    string
		wantErr bool
	}{
		{
			name:    "replaces existing tag",
			content: "images:\n  - name: nginx\n    newTag: v1.0.0\n",
			newTag:  "v2.0.0",
			want:    "images:\n  - name: nginx\n    newTag: v2.0.0\n",
		},
		{
			name:    "preserves extra spaces after colon",
			content: "newTag:   v1.0.0\n",
			newTag:  "v2.0.0",
			want:    "newTag:   v2.0.0\n",
		},
		{
			name:    "sha-style tag",
			content: "newTag: abc123\n",
			newTag:  "sha-deadbeef",
			want:    "newTag: sha-deadbeef\n",
		},
		{
			name:    "no newTag returns error",
			content: "images:\n  - name: nginx\n",
			newTag:  "v2.0.0",
			wantErr: true,
		},
		{
			name:    "empty content returns error",
			content: "",
			newTag:  "v1.0.0",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := updateNewTag(tc.content, tc.newTag)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- GraphQL test helpers ---

// graphqlReq is the request envelope POSTed to /graphql by the client.
type graphqlReq struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// decodeGraphQL reads and decodes a GraphQL request body.
func decodeGraphQL(t *testing.T, r *http.Request) graphqlReq {
	t.Helper()
	var req graphqlReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode graphql request: %v", err)
	}
	return req
}

// writeGraphQL writes a GraphQL response with the given data payload.
func writeGraphQL(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// --- CreateDeployPR ---

func TestCreateDeployPR(t *testing.T) {
	const (
		org           = "test-org"
		repo          = "test-repo"
		app           = "myapp"
		tag           = "v2.0.0"
		baseBranch    = "main"
		baseSHA       = "deadbeef"
		kustomizePath = "apps/myapp/kustomization.yaml"
	)

	initialContent := "images:\n  - name: nginx\n    newTag: v1.0.0\n"

	var capturedAdditions []map[string]string
	var capturedPRTitle, capturedPRBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/graphql") {
			t.Errorf("unexpected request: %s %s — CreateDeployPR should only call /graphql now", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		req := decodeGraphQL(t, r)
		switch {
		case strings.Contains(req.Query, "query DeployState"):
			writeGraphQL(w, map[string]any{
				"repository": map[string]any{
					"id":        "REPOID",
					"baseRef":   map[string]any{"target": map[string]any{"oid": baseSHA}},
					"deployRef": nil,
					"file":      map[string]any{"text": initialContent},
				},
			})
		case strings.Contains(req.Query, "mutation DeployCommit"):
			additions, _ := req.Variables["additions"].([]any)
			for _, a := range additions {
				if m, ok := a.(map[string]any); ok {
					capturedAdditions = append(capturedAdditions, map[string]string{
						"path":     fmt.Sprint(m["path"]),
						"contents": fmt.Sprint(m["contents"]),
					})
				}
			}
			capturedPRTitle, _ = req.Variables["prTitle"].(string)
			capturedPRBody, _ = req.Variables["prBody"].(string)
			writeGraphQL(w, map[string]any{
				"createRef":            map[string]any{"ref": map[string]any{"name": "deploy/dev-myapp-v2.0.0"}},
				"createCommitOnBranch": map[string]any{"commit": map[string]any{"oid": "newcommit"}},
				"createPullRequest": map[string]any{
					"pullRequest": map[string]any{
						"number": 1,
						"url":    "https://github.com/test-org/test-repo/pull/1",
					},
				},
			})
		default:
			t.Errorf("unexpected graphql operation: %s", req.Query)
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewClientWithHTTP(&http.Client{}, server.URL+"/", org, repo)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	prNumber, prURL, err := client.CreateDeployPR(context.Background(), CreatePRParams{
		App:              app,
		Environment:      "dev",
		Tag:              tag,
		KustomizePath:    kustomizePath,
		BaseBranch:       baseBranch,
		Requester:        "deployer",
		Reason:           "release v2",
		RequesterSlackID: "U123",
	})
	if err != nil {
		t.Fatalf("CreateDeployPR: %v", err)
	}

	if prNumber != 1 {
		t.Errorf("prNumber = %d, want 1", prNumber)
	}
	if prURL == "" {
		t.Error("prURL is empty")
	}

	// The kustomize file must be updated with the new tag. Decode the
	// base64 contents from the captured GraphQL FileAddition.
	if len(capturedAdditions) != 1 {
		t.Fatalf("expected 1 file addition, got %d", len(capturedAdditions))
	}
	decoded, err := base64.StdEncoding.DecodeString(capturedAdditions[0]["contents"])
	if err != nil {
		t.Fatalf("decode addition contents: %v", err)
	}
	capturedUpdateContent := string(decoded)
	if !strings.Contains(capturedUpdateContent, "newTag: "+tag) {
		t.Errorf("committed content missing newTag: %s\ngot: %q", tag, capturedUpdateContent)
	}
	if strings.Contains(capturedUpdateContent, "newTag: v1.0.0") {
		t.Errorf("committed content still contains old tag v1.0.0: %q", capturedUpdateContent)
	}
	if got := capturedAdditions[0]["path"]; got != kustomizePath {
		t.Errorf("addition path = %q, want %q", got, kustomizePath)
	}

	// PR title follows the conventional commit format.
	if !strings.Contains(capturedPRTitle, app) || !strings.Contains(capturedPRTitle, tag) {
		t.Errorf("PR title %q should contain app %q and tag %q", capturedPRTitle, app, tag)
	}

	// PR body embeds the recovery metadata comment.
	if !strings.Contains(capturedPRBody, "deploy-bot-meta") {
		t.Errorf("PR body missing deploy-bot-meta comment: %q", capturedPRBody)
	}
	if !strings.Contains(capturedPRBody, "U123") {
		t.Errorf("PR body missing requester Slack ID U123: %q", capturedPRBody)
	}
}

// TestCreateDeployPR_BranchNameSanitized verifies that special characters in
// the tag are replaced before the branch ref is created.
func TestCreateDeployPR_BranchNameSanitized(t *testing.T) {
	const (
		org           = "test-org"
		repo          = "test-repo"
		kustomizePath = "kustomization.yaml"
	)

	var createdBranch string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/graphql") {
			http.NotFound(w, r)
			return
		}
		req := decodeGraphQL(t, r)
		switch {
		case strings.Contains(req.Query, "query DeployState"):
			writeGraphQL(w, map[string]any{
				"repository": map[string]any{
					"id":      "REPOID",
					"baseRef": map[string]any{"target": map[string]any{"oid": "abc"}},
					"file":    map[string]any{"text": "newTag: v1.0.0\n"},
				},
			})
		case strings.Contains(req.Query, "mutation DeployCommit"):
			createdBranch, _ = req.Variables["headBranch"].(string)
			writeGraphQL(w, map[string]any{
				"createPullRequest": map[string]any{
					"pullRequest": map[string]any{"number": 1, "url": "https://example/pr/1"},
				},
			})
		}
	}))
	t.Cleanup(server.Close)

	client, _ := NewClientWithHTTP(&http.Client{}, server.URL+"/", org, repo)

	// Tags with unsafe characters are now rejected before branch creation.
	_, _, err := client.CreateDeployPR(context.Background(), CreatePRParams{
		App: "myapp", Environment: "dev", Tag: "feature/v1.0:arm64", KustomizePath: kustomizePath,
		BaseBranch: "main",
	})
	if err == nil {
		t.Error("expected error for unsafe tag, got nil")
	}

	// Safe tags work and produce a clean branch name.
	createdBranch = ""
	client.CreateDeployPR(context.Background(), CreatePRParams{
		App: "myapp", Environment: "dev", Tag: "v1.0.0-rc.1", KustomizePath: kustomizePath,
		BaseBranch: "main",
	})

	const wantPrefix = "deploy/dev-myapp-"
	if !strings.HasPrefix(createdBranch, wantPrefix) {
		t.Errorf("branch name %q should start with %q", createdBranch, wantPrefix)
	}
	tagSuffix := strings.TrimPrefix(createdBranch, wantPrefix)
	if strings.ContainsAny(tagSuffix, "/:+") {
		t.Errorf("tag portion of branch name %q contains illegal characters", createdBranch)
	}
}

// TestCreateDeployPR_NoChange verifies that CreateDeployPR returns ErrNoChange
// (and deletes the created branch) when the kustomization file already contains
// the requested tag. With the GraphQL implementation no branch is ever
// created (no orphan to clean up).
func TestCreateDeployPR_NoChange(t *testing.T) {
	const (
		org           = "test-org"
		repo          = "test-repo"
		kustomizePath = "apps/myapp/kustomization.yaml"
		currentTag    = "v2.0.0"
	)

	// Content already has the tag we're deploying.
	alreadyCurrent := "images:\n  - name: nginx\n    newTag: " + currentTag + "\n"

	var mutationCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/graphql") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		req := decodeGraphQL(t, r)
		switch {
		case strings.Contains(req.Query, "query DeployState"):
			writeGraphQL(w, map[string]any{
				"repository": map[string]any{
					"id":      "REPOID",
					"baseRef": map[string]any{"target": map[string]any{"oid": "abc"}},
					"file":    map[string]any{"text": alreadyCurrent},
				},
			})
		case strings.Contains(req.Query, "mutation"):
			mutationCalled = true
			t.Errorf("unexpected mutation in no-op path: %s", req.Query)
		}
	}))
	t.Cleanup(server.Close)

	client, _ := NewClientWithHTTP(&http.Client{}, server.URL+"/", org, repo)
	_, _, err := client.CreateDeployPR(context.Background(), CreatePRParams{
		App: "myapp", Environment: "dev", Tag: currentTag,
		KustomizePath: kustomizePath, BaseBranch: "main",
	})

	if !errors.Is(err, ErrNoChange) {
		t.Errorf("expected ErrNoChange, got %v", err)
	}
	if mutationCalled {
		t.Error("no mutation should fire when tag is already current")
	}
}

// TestMergePR_ConflictReturnsErrMergeConflict verifies that MergePR wraps a
// GitHub 405 response as ErrMergeConflict.
func TestMergePR_ConflictReturnsErrMergeConflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pulls/") && strings.HasSuffix(r.URL.Path, "/merge") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed) // 405
			json.NewEncoder(w).Encode(map[string]interface{}{
				"message": "Pull Request is not mergeable",
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	client, _ := NewClientWithHTTP(&http.Client{}, server.URL+"/", "org", "repo")
	err := client.MergePR(context.Background(), 1, "squash")
	if !errors.Is(err, ErrMergeConflict) {
		t.Errorf("expected ErrMergeConflict, got %v", err)
	}
}

// TestMergePR_405MessageParsing verifies that the 405 message field is used to
// distinguish ErrCINotPassed, ErrDraftPR, and ErrMergeConflict.
func TestMergePR_405MessageParsing(t *testing.T) {
	cases := []struct {
		name    string
		message string
		wantErr error
	}{
		{"status check keyword", "Required status check has not passed", ErrCINotPassed},
		{"required keyword", "1 required status check is failing", ErrCINotPassed},
		{"draft keyword", "Pull request is in draft state", ErrDraftPR},
		{"conflict default", "Pull Request is not mergeable", ErrMergeConflict},
		{"branch out of date", "Base branch is out of date", ErrMergeConflict},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/pulls/") && strings.HasSuffix(r.URL.Path, "/merge") {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusMethodNotAllowed) // 405
					json.NewEncoder(w).Encode(map[string]interface{}{
						"message": tc.message,
					})
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(server.Close)

			client, _ := NewClientWithHTTP(&http.Client{}, server.URL+"/", "org", "repo")
			err := client.MergePR(context.Background(), 1, "squash")
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("message %q: expected %v, got %v", tc.message, tc.wantErr, err)
			}
		})
	}
}

// TestMergePR_409ReturnsErrHeadModified verifies that a 409 response maps to
// ErrHeadModified.
func TestMergePR_409ReturnsErrHeadModified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pulls/") && strings.HasSuffix(r.URL.Path, "/merge") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict) // 409
			json.NewEncoder(w).Encode(map[string]interface{}{
				"message": "Head branch was modified. Review and try the merge again.",
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	client, _ := NewClientWithHTTP(&http.Client{}, server.URL+"/", "org", "repo")
	err := client.MergePR(context.Background(), 1, "squash")
	if !errors.Is(err, ErrHeadModified) {
		t.Errorf("expected ErrHeadModified, got %v", err)
	}
}

// TestRebaseDeployBranch_Success verifies that RebaseDeployBranch issues the
// expected GraphQL query and the rebase mutation (updateRef force +
// createCommitOnBranch) and writes the new tag into the file addition.
func TestRebaseDeployBranch_Success(t *testing.T) {
	const (
		org           = "test-org"
		repo          = "test-repo"
		kustomizePath = "apps/myapp/kustomization.yaml"
		baseSHA       = "headsha"
		deployRefID   = "DEPLOYREFID"
	)

	currentContent := "newTag: v1.0.0\n"
	targetTag := "v2.0.0"

	var (
		mutationCalled  bool
		capturedRefID   string
		capturedBaseSHA string
		capturedFile    string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/graphql") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		req := decodeGraphQL(t, r)
		switch {
		case strings.Contains(req.Query, "query DeployState"):
			writeGraphQL(w, map[string]any{
				"repository": map[string]any{
					"id":        "REPOID",
					"baseRef":   map[string]any{"target": map[string]any{"oid": baseSHA}},
					"deployRef": map[string]any{"id": deployRefID},
					"file":      map[string]any{"text": currentContent},
				},
			})
		case strings.Contains(req.Query, "mutation Rebase"):
			mutationCalled = true
			capturedRefID, _ = req.Variables["refId"].(string)
			capturedBaseSHA, _ = req.Variables["baseSHA"].(string)
			additions, _ := req.Variables["additions"].([]any)
			if len(additions) == 1 {
				if m, ok := additions[0].(map[string]any); ok {
					decoded, _ := base64.StdEncoding.DecodeString(fmt.Sprint(m["contents"]))
					capturedFile = string(decoded)
				}
			}
			writeGraphQL(w, map[string]any{
				"updateRef":            map[string]any{"ref": map[string]any{"name": "deploy/dev-myapp-v2.0.0"}},
				"createCommitOnBranch": map[string]any{"commit": map[string]any{"oid": "newcommit"}},
			})
		default:
			t.Errorf("unexpected graphql operation: %s", req.Query)
		}
	}))
	t.Cleanup(server.Close)

	client, _ := NewClientWithHTTP(&http.Client{}, server.URL+"/", org, repo)
	err := client.RebaseDeployBranch(context.Background(), CreatePRParams{
		App: "myapp", Environment: "dev", Tag: targetTag,
		KustomizePath: kustomizePath, BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("RebaseDeployBranch: %v", err)
	}

	if !mutationCalled {
		t.Error("expected rebase mutation to be called")
	}
	if capturedRefID != deployRefID {
		t.Errorf("refId = %q, want %q", capturedRefID, deployRefID)
	}
	if capturedBaseSHA != baseSHA {
		t.Errorf("baseSHA = %q, want %q", capturedBaseSHA, baseSHA)
	}
	if !strings.Contains(capturedFile, "newTag: "+targetTag) {
		t.Errorf("addition contents missing new tag: %q", capturedFile)
	}
}

// TestRebaseDeployBranch_AlreadyCurrent verifies that RebaseDeployBranch returns
// ErrNoChange when the target tag is already on the base branch — and that
// the rebase mutation is NOT called in that case.
func TestRebaseDeployBranch_AlreadyCurrent(t *testing.T) {
	const currentTag = "v2.0.0"
	alreadyCurrent := "newTag: " + currentTag + "\n"

	var mutationCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/graphql") {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		req := decodeGraphQL(t, r)
		switch {
		case strings.Contains(req.Query, "query DeployState"):
			writeGraphQL(w, map[string]any{
				"repository": map[string]any{
					"id":        "REPOID",
					"baseRef":   map[string]any{"target": map[string]any{"oid": "abc"}},
					"deployRef": map[string]any{"id": "DEPLOYREFID"},
					"file":      map[string]any{"text": alreadyCurrent},
				},
			})
		case strings.Contains(req.Query, "mutation"):
			mutationCalled = true
			t.Errorf("unexpected mutation in no-op rebase path: %s", req.Query)
		}
	}))
	t.Cleanup(server.Close)

	client, _ := NewClientWithHTTP(&http.Client{}, server.URL+"/", "org", "repo")
	err := client.RebaseDeployBranch(context.Background(), CreatePRParams{
		App: "myapp", Environment: "dev", Tag: currentTag,
		KustomizePath: "kustomization.yaml", BaseBranch: "main",
	})
	if !errors.Is(err, ErrNoChange) {
		t.Errorf("expected ErrNoChange, got %v", err)
	}
	if mutationCalled {
		t.Error("no mutation should fire when tag is already current")
	}
}

// TestClosePR_AlreadyClosed verifies that ClosePR returns nil on 404 and 422
// responses (PR already closed or not found — goal already achieved).
func TestClosePR_AlreadyClosed(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]string{"message": "already closed"})
			}))
			t.Cleanup(server.Close)

			client, _ := NewClientWithHTTP(&http.Client{}, server.URL+"/", "org", "repo")
			if err := client.ClosePR(context.Background(), 42); err != nil {
				t.Errorf("status %d: expected nil, got %v", status, err)
			}
		})
	}
}

// TestDeleteBranch_AlreadyGone verifies that DeleteBranch returns nil on 422
// (GitHub "Reference does not exist" — branch already deleted).
func TestDeleteBranch_AlreadyGone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity) // 422
		json.NewEncoder(w).Encode(map[string]string{"message": "Reference does not exist"})
	}))
	t.Cleanup(server.Close)

	client, _ := NewClientWithHTTP(&http.Client{}, server.URL+"/", "org", "repo")
	if err := client.DeleteBranch(context.Background(), "deploy/prod-myapp-v1.0.0"); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}
