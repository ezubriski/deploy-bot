package bot

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
	githubpkg "github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/store"
)

func newECRTestBot(t *testing.T, gh githubClient, sl *captureSlack, st *store.Store, apps []config.AppConfig) *Bot {
	t.Helper()
	cfg := &config.Config{
		Slack:      config.SlackConfig{DeployChannel: "C_DEPLOY"},
		Deployment: config.DeploymentConfig{MergeMethod: "squash", LockTTL: "5m", StaleDuration: "2h"},
		Apps:       apps,
	}
	return &Bot{
		slack:     sl,
		store:     st,
		gh:        gh,
		ecrCache:  stubECR{},
		validator: stubValidator{},
		auditLog:  nopAudit{},
		metrics:   metrics.New(prometheus.NewRegistry()),
		cfg:       config.NewHolder(cfg, ""),
		log:       zap.NewNop(),
	}
}

func TestHandleECRPush_AppNotFound(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newECRTestBot(t, &stubGH{}, sl, st, nil)

	b.handleECRPush(context.Background(), queue.ECRPushEvent{
		App: "nonexistent", Tag: "v1.0.0", Repository: "myrepo",
	})

	// No Slack messages should be posted for unknown app.
	if sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected no Slack message for unknown app")
	}
}

func TestHandleECRPush_Locked(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	apps := []config.AppConfig{{App: "myapp", Environment: "dev", KustomizePath: "apps/myapp/kustomization.yaml"}}
	b := newECRTestBot(t, &stubGH{}, sl, st, apps)

	ctx := context.Background()
	// Pre-acquire lock.
	_, _ = st.AcquireLock(ctx, "dev", "myapp", "someone", 5*60_000_000_000)

	b.handleECRPush(ctx, queue.ECRPushEvent{
		App: "myapp", Tag: "v1.0.0", Repository: "myrepo",
	})

	// No PR should be created when locked.
	if sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected no Slack message when app is locked")
	}
}

func TestHandleECRPush_NoOp(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	apps := []config.AppConfig{{App: "myapp", Environment: "dev", KustomizePath: "apps/myapp/kustomization.yaml"}}

	gh := &stubGH{
		createDeployPR: func(_ context.Context, _ githubpkg.CreatePRParams) (int, string, error) {
			return 0, "", githubpkg.ErrNoChange
		},
	}
	b := newECRTestBot(t, gh, sl, st, apps)

	b.handleECRPush(context.Background(), queue.ECRPushEvent{
		App: "myapp", Tag: "v1.0.0", Repository: "myrepo",
	})

	// Should post a no-op notice.
	if !sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected no-op notice to deploy channel")
	}

	// Lock should be released.
	locked, err := st.IsLocked(context.Background(), "dev", "myapp")
	if err != nil {
		t.Fatal(err)
	}
	if locked {
		t.Error("lock should be released after no-op")
	}
}

func TestHandleECRPush_ApprovalRequired(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	apps := []config.AppConfig{{
		App: "myapp", Environment: "dev",
		KustomizePath: "apps/myapp/kustomization.yaml",
		AutoDeploy:    false,
	}}

	b := newECRTestBot(t, &stubGH{}, sl, st, apps)

	b.handleECRPush(context.Background(), queue.ECRPushEvent{
		App: "myapp", Tag: "v1.0.0", Repository: "myrepo",
	})

	// Should post approval request.
	if !sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected approval request to deploy channel")
	}

	// PendingDeploy should be stored.
	d, err := st.Get(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil {
		t.Fatal("expected pending deploy in store")
	}
	if d.Requester != "ECR" {
		t.Errorf("requester = %q, want %q", d.Requester, "ECR")
	}
	if d.State != store.StatePending {
		t.Errorf("state = %q, want %q", d.State, store.StatePending)
	}
}

func TestHandleECRPush_AutoDeploy(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	apps := []config.AppConfig{{
		App: "myapp", Environment: "dev",
		KustomizePath: "apps/myapp/kustomization.yaml",
		AutoDeploy:    true,
	}}

	merged := false
	gh := &stubGH{
		mergePR: func(_ context.Context, _ int, _ string) error {
			merged = true
			return nil
		},
	}
	b := newECRTestBot(t, gh, sl, st, apps)

	b.handleECRPush(context.Background(), queue.ECRPushEvent{
		App: "myapp", Tag: "v1.0.0", Repository: "myrepo",
	})

	if !merged {
		t.Error("expected PR to be merged for auto-deploy")
	}
	if !sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected completion notice to deploy channel")
	}

	// Lock should be released after auto-deploy.
	locked, err := st.IsLocked(context.Background(), "dev", "myapp")
	if err != nil {
		t.Fatal(err)
	}
	if locked {
		t.Error("lock should be released after auto-deploy")
	}
}

func TestHandleECRPush_ProdGuard(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	apps := []config.AppConfig{{
		App: "myapp", Environment: "prod",
		KustomizePath: "apps/myapp/kustomization.yaml",
		AutoDeploy:    true, // auto_deploy is true but prod guard blocks it
	}}

	merged := false
	gh := &stubGH{
		mergePR: func(_ context.Context, _ int, _ string) error {
			merged = true
			return nil
		},
	}
	b := newECRTestBot(t, gh, sl, st, apps)

	b.handleECRPush(context.Background(), queue.ECRPushEvent{
		App: "myapp", Tag: "v1.0.0", Repository: "myrepo",
	})

	// Prod guard should prevent auto-deploy — should fall back to approval.
	if merged {
		t.Error("expected prod guard to prevent auto-merge")
	}

	// Should still post an approval request.
	if !sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected approval request to deploy channel")
	}

	// PendingDeploy should be stored (approval-required path).
	d, err := st.Get(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil {
		t.Fatal("expected pending deploy in store")
	}
}

func TestHandleECRPush_ApproverGroupChannel(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	apps := []config.AppConfig{{
		App: "myapp", Environment: "dev",
		KustomizePath:          "apps/myapp/kustomization.yaml",
		AutoDeployApproverGroup: "C_APPROVERS",
	}}

	b := newECRTestBot(t, &stubGH{}, sl, st, apps)

	b.handleECRPush(context.Background(), queue.ECRPushEvent{
		App: "myapp", Tag: "v1.0.0", Repository: "myrepo",
	})

	// Should post to the approver channel, not deploy channel.
	if !sl.hasMessageTo("C_APPROVERS") {
		t.Error("expected approval request to approver channel")
	}
}
