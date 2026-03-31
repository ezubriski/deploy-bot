package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// --- sanitizeBranchName ---

func TestSanitizeBranchName(t *testing.T) {
	cases := []struct {
		tag  string
		want string
	}{
		{"v1.2.3", "v1.2.3"},
		{"sha-abc123", "sha-abc123"},
		{"feature/my-tag", "feature-my-tag"},
		{"v1.0.0:arm64", "v1.0.0-arm64"},
		{"v1.0.0+build.1", "v1.0.0-build.1"},
		{"my tag", "my-tag"},
		{"a/b:c+d e", "a-b-c-d-e"},
	}
	for _, tc := range cases {
		got := sanitizeBranchName(tc.tag)
		if got != tc.want {
			t.Errorf("sanitizeBranchName(%q) = %q, want %q", tc.tag, got, tc.want)
		}
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
				"ref":    "refs/heads/deploy/myapp-v2.0.0",
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
	client.CreateDeployPR(context.Background(), CreatePRParams{
		App: "myapp", Tag: "feature/v1.0:arm64", KustomizePath: kustomizePath,
		BaseBranch: "main",
	})

	const wantPrefix = "deploy/myapp-"
	if !strings.HasPrefix(createdBranch, wantPrefix) {
		t.Errorf("branch name %q should start with %q", createdBranch, wantPrefix)
	}
	// Only the tag suffix (after the deploy/<app>- prefix) must be free of
	// special characters that are illegal in git ref names.
	tagSuffix := strings.TrimPrefix(createdBranch, wantPrefix)
	if strings.ContainsAny(tagSuffix, "/:") {
		t.Errorf("tag portion of branch name %q contains illegal characters (/ or :)", createdBranch)
	}
}
