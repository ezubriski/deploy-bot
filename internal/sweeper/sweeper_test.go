package sweeper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
	githubpkg "github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/store"
)

// --- helpers ---

func newTestStore(t *testing.T) (*store.Store, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return store.New(mr.Addr(), ""), mr
}

func newTestCfgHolder() *config.Holder {
	cfg := &config.Config{
		GitHub: config.GitHubConfig{Org: "org", Repo: "repo"},
		Deployment: config.DeploymentConfig{
			Label:       "deploy-bot",
			MergeMethod: "squash",
		},
		Apps: []config.AppConfig{
			{App: "myapp", Environment: "dev", KustomizePath: "apps/myapp/kustomization.yaml"},
		},
	}
	return config.NewHolder(cfg, "/tmp/fake")
}

// prIssueJSON builds a GitHub API issue response representing a deploy-bot PR.
func prIssueJSON(number int, app, tag, requesterID string) map[string]interface{} {
	meta := fmt.Sprintf(`{"requester_id":%q,"app":%q,"tag":%q,"environment":"dev"}`, requesterID, app, tag)
	body := fmt.Sprintf("Deploy PR\n<!-- deploy-bot-meta: %s -->", meta)
	return map[string]interface{}{
		"number":   number,
		"html_url": fmt.Sprintf("https://github.com/org/repo/pull/%d", number),
		"body":     body,
		"state":    "open",
		"pull_request": map[string]interface{}{
			"url": fmt.Sprintf("https://api.github.com/repos/org/repo/pulls/%d", number),
		},
	}
}

// --- ReconcileFromGitHub ---

func TestReconcileFromGitHub_RehydratesUntrackedPRs(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	// PR #1 is tracked in Redis — should not be touched.
	_ = st.Set(ctx, &store.PendingDeploy{
		App:       "myapp",
		Tag:       "v1.0.0",
		PRNumber:  1,
		State:     store.StatePending,
		ExpiresAt: time.Now().Add(time.Hour),
	}, time.Hour)

	// PR #2 is not in Redis — should be re-hydrated.
	var labelAdded atomic.Bool

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			// ListOpenPRsWithLabel
			json.NewEncoder(w).Encode([]interface{}{
				prIssueJSON(1, "myapp", "v1.0.0", "U123"),
				prIssueJSON(2, "myapp", "v1.1.0", "U456"),
			})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/labels"):
			// AddLabels
			labelAdded.Store(true)
			json.NewEncoder(w).Encode([]interface{}{})
		default:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{})
		}
	}))
	t.Cleanup(ghServer.Close)

	ghClient, err := githubpkg.NewClientWithHTTP(&http.Client{}, ghServer.URL+"/", "org", "repo")
	if err != nil {
		t.Fatalf("create github client: %v", err)
	}

	sw := New(st, ghClient, nil, nil, nil, newTestCfgHolder(), zap.NewNop())
	sw.ReconcileFromGitHub(ctx)

	// PR #2 should now be in Redis.
	d, err := st.Get(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil {
		t.Fatal("expected PR #2 to be re-hydrated in Redis")
	}
	if d.App != "myapp" || d.Tag != "v1.1.0" {
		t.Errorf("re-hydrated deploy = %+v", d)
	}
	if d.State != store.StatePending {
		t.Errorf("state = %q, want pending", d.State)
	}

	// Pending label should have been added.
	if !labelAdded.Load() {
		t.Error("expected pending label to be added to re-hydrated PR")
	}

	// PR #1 should still be in Redis unchanged.
	d1, _ := st.Get(ctx, 1)
	if d1 == nil || d1.Tag != "v1.0.0" {
		t.Error("PR #1 should be unchanged")
	}
}

func TestReconcileFromGitHub_NoPRsMissing(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	// Both PRs are tracked in Redis.
	for _, prNum := range []int{1, 2} {
		_ = st.Set(ctx, &store.PendingDeploy{
			App: "myapp", Tag: "v1.0.0", PRNumber: prNum,
			State: store.StatePending, ExpiresAt: time.Now().Add(time.Hour),
		}, time.Hour)
	}

	var patchCalled atomic.Bool

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues") {
			json.NewEncoder(w).Encode([]interface{}{
				prIssueJSON(1, "myapp", "v1.0.0", "U123"),
				prIssueJSON(2, "myapp", "v1.1.0", "U456"),
			})
			return
		}
		if r.Method == http.MethodPatch {
			patchCalled.Store(true)
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ghServer.Close)

	ghClient, _ := githubpkg.NewClientWithHTTP(&http.Client{}, ghServer.URL+"/", "org", "repo")
	sw := New(st, ghClient, nil, nil, nil, newTestCfgHolder(), zap.NewNop())
	sw.ReconcileFromGitHub(ctx)

	if patchCalled.Load() {
		t.Error("expected no PRs to be closed when all are tracked in Redis")
	}
}

// --- ReconstructHistory ---

func TestReconstructHistory_SkipsWhenHistoryExists(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	_ = st.PushHistory(ctx, store.HistoryEntry{
		EventType: "approved", App: "myapp", Tag: "v1.0.0", CompletedAt: time.Now(),
	})

	var githubCalled atomic.Bool
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		githubCalled.Store(true)
		http.NotFound(w, r)
	}))
	t.Cleanup(ghServer.Close)

	ghClient, _ := githubpkg.NewClientWithHTTP(&http.Client{}, ghServer.URL+"/", "org", "repo")
	sw := New(st, ghClient, nil, nil, nil, newTestCfgHolder(), zap.NewNop())
	sw.ReconstructHistory(ctx)

	if githubCalled.Load() {
		t.Error("expected GitHub API not to be called when history already exists")
	}
}

func TestReconstructHistory_PopulatesFromCommits(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/commits/") && strings.Contains(r.URL.Path, "/pulls"):
			// PRForCommit: GET /repos/org/repo/commits/{sha}/pulls
			json.NewEncoder(w).Encode([]interface{}{
				map[string]interface{}{"number": 42, "html_url": "https://github.com/org/repo/pull/42"},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/commits"):
			// ListDeployCommits: GET /repos/org/repo/commits?path=...
			json.NewEncoder(w).Encode([]interface{}{
				map[string]interface{}{
					"sha": "abc123",
					"commit": map[string]interface{}{
						"message": "deploy(myapp): update image tag to v1.2.3",
						"committer": map[string]interface{}{
							"date": "2024-06-01T12:00:00Z",
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ghServer.Close)

	ghClient, err := githubpkg.NewClientWithHTTP(&http.Client{}, ghServer.URL+"/", "org", "repo")
	if err != nil {
		t.Fatalf("create github client: %v", err)
	}

	sw := New(st, ghClient, nil, nil, nil, newTestCfgHolder(), zap.NewNop())
	sw.ReconstructHistory(ctx)

	entries, err := st.GetHistory(ctx, 10)
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(entries))
	}
	e := entries[0]
	if e.App != "myapp" {
		t.Errorf("App = %q, want %q", e.App, "myapp")
	}
	if e.Tag != "v1.2.3" {
		t.Errorf("Tag = %q, want %q", e.Tag, "v1.2.3")
	}
	if e.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", e.PRNumber)
	}
	if e.PRURL != "https://github.com/org/repo/pull/42" {
		t.Errorf("PRURL = %q, want %q", e.PRURL, "https://github.com/org/repo/pull/42")
	}
}
