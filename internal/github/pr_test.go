package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// --- CreateDeployPR ---

func TestCreateDeployPR(t *testing.T) {
	const (
		org           = "test-org"
		repo          = "test-repo"
		app           = "myapp"
		tag           = "v2.0.0"
		baseBranch    = "main"
		baseSHA       = "deadbeef"
		fileSHA       = "cafebabe"
		kustomizePath = "apps/myapp/kustomization.yaml"
	)

	initialContent := "images:\n  - name: nginx\n    newTag: v1.0.0\n"

	var capturedUpdateContent string
	var capturedPRTitle, capturedPRBody string
	var labelsCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		switch {
		// GetRef — return base branch SHA
		case r.Method == http.MethodGet && strings.Contains(path, "/git/ref/heads/"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ref": "refs/heads/" + baseBranch,
				"object": map[string]interface{}{
					"sha":  baseSHA,
					"type": "commit",
				},
			})

		// CreateRef — create new branch
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/refs"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ref":    "refs/heads/deploy/dev-myapp-v2.0.0",
				"object": map[string]interface{}{"sha": baseSHA},
			})

		// GetContents — return initial kustomization file
		case r.Method == http.MethodGet && strings.Contains(path, "/contents/"):
			encoded := base64.StdEncoding.EncodeToString([]byte(initialContent))
			json.NewEncoder(w).Encode(map[string]interface{}{
				"type":     "file",
				"encoding": "base64",
				"content":  encoded,
				"sha":      fileSHA,
				"name":     "kustomization.yaml",
				"path":     kustomizePath,
			})

		// UpdateFile — capture the committed content
		case r.Method == http.MethodPut && strings.Contains(path, "/contents/"):
			var body struct {
				Content string `json:"content"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			decoded, _ := base64.StdEncoding.DecodeString(body.Content)
			capturedUpdateContent = string(decoded)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"content": map[string]interface{}{"sha": "newsha"},
				"commit":  map[string]interface{}{"sha": "commitsha"},
			})

		// CreatePR — capture title/body, return PR #1
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/pulls"):
			var body struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			capturedPRTitle = body.Title
			capturedPRBody = body.Body
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number":   1,
				"html_url": "https://github.com/test-org/test-repo/pull/1",
				"title":    body.Title,
			})

		// AddLabels
		case r.Method == http.MethodPost && strings.Contains(path, "/labels"):
			labelsCalled = true
			json.NewEncoder(w).Encode([]interface{}{
				map[string]interface{}{"name": "deploy-bot"},
			})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
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
		Labels:           []string{"deploy-bot"},
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

	// The kustomize file must be updated with the new tag.
	if !strings.Contains(capturedUpdateContent, "newTag: "+tag) {
		t.Errorf("committed content missing newTag: %s\ngot: %q", tag, capturedUpdateContent)
	}
	if strings.Contains(capturedUpdateContent, "newTag: v1.0.0") {
		t.Errorf("committed content still contains old tag v1.0.0: %q", capturedUpdateContent)
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

	if !labelsCalled {
		t.Error("expected labels to be applied to the PR")
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
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/git/ref/heads/"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ref":    "refs/heads/main",
				"object": map[string]interface{}{"sha": "abc", "type": "commit"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/refs"):
			var body struct {
				Ref string `json:"ref"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			createdBranch = strings.TrimPrefix(body.Ref, "refs/heads/")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ref":    body.Ref,
				"object": map[string]interface{}{"sha": "abc"},
			})
		case r.Method == http.MethodGet && strings.Contains(path, "/contents/"):
			encoded := base64.StdEncoding.EncodeToString([]byte("newTag: v1.0.0\n"))
			json.NewEncoder(w).Encode(map[string]interface{}{
				"type": "file", "encoding": "base64",
				"content": encoded, "sha": "sha1",
			})
		case r.Method == http.MethodPut && strings.Contains(path, "/contents/"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"content": map[string]interface{}{"sha": "s"},
				"commit":  map[string]interface{}{"sha": "c"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/pulls"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number":   1,
				"html_url": "https://github.com/test-org/test-repo/pull/1",
			})
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{})
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
// the requested tag.
func TestCreateDeployPR_NoChange(t *testing.T) {
	const (
		org           = "test-org"
		repo          = "test-repo"
		kustomizePath = "apps/myapp/kustomization.yaml"
		currentTag    = "v2.0.0"
	)

	// Content already has the tag we're deploying.
	alreadyCurrent := "images:\n  - name: nginx\n    newTag: " + currentTag + "\n"

	var branchCreated, branchDeleted bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/git/ref/heads/"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ref":    "refs/heads/main",
				"object": map[string]interface{}{"sha": "abc", "type": "commit"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/refs"):
			branchCreated = true
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ref":    "refs/heads/deploy/dev-myapp-v2.0.0",
				"object": map[string]interface{}{"sha": "abc"},
			})
		case r.Method == http.MethodGet && strings.Contains(path, "/contents/"):
			encoded := base64.StdEncoding.EncodeToString([]byte(alreadyCurrent))
			json.NewEncoder(w).Encode(map[string]interface{}{
				"type": "file", "encoding": "base64",
				"content": encoded, "sha": "sha1",
			})
		case r.Method == http.MethodDelete && strings.Contains(path, "/git/refs/"):
			branchDeleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
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
	if !branchCreated {
		t.Error("expected branch to be created before no-op detection")
	}
	if !branchDeleted {
		t.Error("expected created branch to be deleted on no-op")
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

// TestRebaseDeployBranch_Success verifies that RebaseDeployBranch calls the
// full Git Data API sequence and force-updates the deploy branch ref.
func TestRebaseDeployBranch_Success(t *testing.T) {
	const (
		org           = "test-org"
		repo          = "test-repo"
		kustomizePath = "apps/myapp/kustomization.yaml"
		headSHA       = "headsha"
		treeSHA       = "treesha"
		blobSHA       = "blobsha"
		newTreeSHA    = "newtreesha"
		newCommitSHA  = "newcommitsha"
	)

	currentContent := "newTag: v1.0.0\n"
	targetTag := "v2.0.0"

	var (
		blobCreated   bool
		treeCreated   bool
		commitCreated bool
		refUpdated    bool
		forceUpdate   bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		// GetRef — base branch HEAD
		case r.Method == http.MethodGet && strings.Contains(path, "/git/ref/heads/"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ref":    "refs/heads/main",
				"object": map[string]interface{}{"sha": headSHA, "type": "commit"},
			})

		// GetContents — current file at base branch
		case r.Method == http.MethodGet && strings.Contains(path, "/contents/"):
			encoded := base64.StdEncoding.EncodeToString([]byte(currentContent))
			json.NewEncoder(w).Encode(map[string]interface{}{
				"type": "file", "encoding": "base64",
				"content": encoded, "sha": "filesha",
			})

		// CreateBlob
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/blobs"):
			blobCreated = true
			json.NewEncoder(w).Encode(map[string]interface{}{"sha": blobSHA})

		// GetCommit
		case r.Method == http.MethodGet && strings.Contains(path, "/git/commits/"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"sha":  headSHA,
				"tree": map[string]interface{}{"sha": treeSHA},
			})

		// CreateTree
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/trees"):
			treeCreated = true
			json.NewEncoder(w).Encode(map[string]interface{}{"sha": newTreeSHA})

		// CreateCommit
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/git/commits"):
			commitCreated = true
			json.NewEncoder(w).Encode(map[string]interface{}{"sha": newCommitSHA})

		// UpdateRef (PATCH)
		case r.Method == http.MethodPatch && strings.Contains(path, "/git/refs/"):
			refUpdated = true
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if b, ok := body["force"].(bool); ok && b {
				forceUpdate = true
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ref":    "refs/heads/deploy/dev-myapp-v2.0.0",
				"object": map[string]interface{}{"sha": newCommitSHA},
			})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
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

	if !blobCreated {
		t.Error("expected CreateBlob to be called")
	}
	if !treeCreated {
		t.Error("expected CreateTree to be called")
	}
	if !commitCreated {
		t.Error("expected CreateCommit to be called")
	}
	if !refUpdated {
		t.Error("expected UpdateRef to be called")
	}
	if !forceUpdate {
		t.Error("expected force=true on UpdateRef")
	}
}

// TestRebaseDeployBranch_AlreadyCurrent verifies that RebaseDeployBranch returns
// ErrNoChange when the target tag is already on the base branch.
func TestRebaseDeployBranch_AlreadyCurrent(t *testing.T) {
	const currentTag = "v2.0.0"
	alreadyCurrent := "newTag: " + currentTag + "\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/git/ref/heads/"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ref":    "refs/heads/main",
				"object": map[string]interface{}{"sha": "abc", "type": "commit"},
			})
		case r.Method == http.MethodGet && strings.Contains(path, "/contents/"):
			encoded := base64.StdEncoding.EncodeToString([]byte(alreadyCurrent))
			json.NewEncoder(w).Encode(map[string]interface{}{
				"type": "file", "encoding": "base64",
				"content": encoded, "sha": "sha1",
			})
		default:
			// No blob/tree/commit/ref calls should happen.
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
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
