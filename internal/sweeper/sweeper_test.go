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
	"github.com/slack-go/slack"
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

func TestReconcileFromGitHub_ClosesUntrackedPRs(t *testing.T) {
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

	// PR #2 is not in Redis but has a lock — should be closed and lock released.
	_, _ = st.AcquireLock(ctx, "dev", "myapp", "U456", 5*time.Minute)

	var closedPRs []int
	var slackMsgs atomic.Int32

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			// ListOpenPRsWithLabel
			json.NewEncoder(w).Encode([]interface{}{
				prIssueJSON(1, "myapp", "v1.0.0", "U123"),
				prIssueJSON(2, "myapp", "v1.1.0", "U456"),
			})
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/pulls/"):
			// ClosePR
			parts := strings.Split(r.URL.Path, "/")
			var n int
			fmt.Sscanf(parts[len(parts)-1], "%d", &n)
			closedPRs = append(closedPRs, n)
			json.NewEncoder(w).Encode(map[string]interface{}{"number": n, "state": "closed"})
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/labels/"):
			// RemoveLabel
			json.NewEncoder(w).Encode([]interface{}{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ghServer.Close)

	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slackMsgs.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "channel": "U456", "ts": "123.456"})
	}))
	t.Cleanup(slackServer.Close)

	ghClient, err := githubpkg.NewClientWithHTTP(&http.Client{}, ghServer.URL+"/", "org", "repo")
	if err != nil {
		t.Fatalf("create github client: %v", err)
	}
	slackClient := slack.New("test-token", slack.OptionAPIURL(slackServer.URL+"/"))

	sw := New(st, ghClient, slackClient, nil, nil, newTestCfgHolder(), zap.NewNop())
	sw.ReconcileFromGitHub(ctx)

	// Only PR #2 should have been closed.
	if len(closedPRs) != 1 || closedPRs[0] != 2 {
		t.Errorf("closed PRs = %v, want [2]", closedPRs)
	}

	// Lock for myapp must be released (PR #2 held it).
	locked, _ := st.IsLocked(ctx, "dev", "myapp")
	if locked {
		t.Error("expected myapp lock to be released after reconcile")
	}

	// One DM should have been sent for PR #2's requester.
	if n := slackMsgs.Load(); n != 1 {
		t.Errorf("slack messages = %d, want 1", n)
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
