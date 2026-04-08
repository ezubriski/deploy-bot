package bot

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ezubriski/deploy-bot/internal/audit"
	"github.com/ezubriski/deploy-bot/internal/config"
	githubpkg "github.com/ezubriski/deploy-bot/internal/github"
	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/store"
	"github.com/ezubriski/deploy-bot/internal/validator"
)

// --- test doubles ---

// stubGH is a test double for githubClient. Unset fields return nil errors.
type stubGH struct {
	getDefaultBranch   func(context.Context) (string, error)
	createDeployPR     func(context.Context, githubpkg.CreatePRParams) (int, string, error)
	rebaseDeployBranch func(context.Context, githubpkg.CreatePRParams) error
	mergePR            func(context.Context, int, string) error
	closePR            func(context.Context, int) error
	deleteBranch       func(context.Context, string) error
}

func (s *stubGH) GetDefaultBranch(ctx context.Context) (string, error) {
	if s.getDefaultBranch != nil {
		return s.getDefaultBranch(ctx)
	}
	return "main", nil
}
func (s *stubGH) CreateDeployPR(ctx context.Context, p githubpkg.CreatePRParams) (int, string, error) {
	if s.createDeployPR != nil {
		return s.createDeployPR(ctx, p)
	}
	return 1, "https://github.com/org/repo/pull/1", nil
}
func (s *stubGH) RebaseDeployBranch(ctx context.Context, p githubpkg.CreatePRParams) error {
	if s.rebaseDeployBranch != nil {
		return s.rebaseDeployBranch(ctx, p)
	}
	return nil
}
func (s *stubGH) MergePR(ctx context.Context, prNumber int, method string) error {
	if s.mergePR != nil {
		return s.mergePR(ctx, prNumber, method)
	}
	return nil
}
func (s *stubGH) ClosePR(ctx context.Context, prNumber int) error {
	if s.closePR != nil {
		return s.closePR(ctx, prNumber)
	}
	return nil
}
func (s *stubGH) DeleteBranch(ctx context.Context, branch string) error {
	if s.deleteBranch != nil {
		return s.deleteBranch(ctx, branch)
	}
	return nil
}
func (s *stubGH) CommentRequested(_ context.Context, _ int, _, _, _, _ string) error { return nil }
func (s *stubGH) CommentApproved(_ context.Context, _ int, _ string) error           { return nil }
func (s *stubGH) CommentRejected(_ context.Context, _ int, _, _ string) error        { return nil }
func (s *stubGH) CommentExpired(_ context.Context, _ int, _ string) error            { return nil }
func (s *stubGH) CommentCancelled(_ context.Context, _ int, _ string) error          { return nil }
func (s *stubGH) CommentNoOp(_ context.Context, _ int, _, _ string) error            { return nil }
func (s *stubGH) CommentAutoDeployFailed(_ context.Context, _ int, _ error) error    { return nil }
func (s *stubGH) RemoveLabel(_ context.Context, _ int, _ string) error               { return nil }
func (s *stubGH) AddLabels(_ context.Context, _ int, _ []string) error               { return nil }

// stubValidator always authorizes and maps slack IDs to "gh-user".
type stubValidator struct{}

func (stubValidator) IsMember(_ context.Context, _ string) (bool, validator.Identity, error) {
	return true, validator.Identity{GitHubLogin: "gh-member", Email: "member@example.com", Name: "Test Member"}, nil
}
func (stubValidator) ResolveIdentity(_ context.Context, _ string) (validator.Identity, error) {
	return validator.Identity{GitHubLogin: "gh-user", Email: "user@example.com", Name: "Test User"}, nil
}
func (stubValidator) SlackUserToGitHub(_ context.Context, _ string) (string, error) {
	return "gh-user", nil
}

// stubECR considers all tags valid.
type stubECR struct{}

func (stubECR) ValidateTag(_ context.Context, _, _ string) (bool, error) { return true, nil }
func (stubECR) RecentTags(_ string) []string                             { return nil }
func (stubECR) Tags(_ string, _ int) []string                            { return nil }

// captureSlack records channels that receive PostMessageContext calls.
type captureSlack struct {
	mu       sync.Mutex
	channels []string
}

func (c *captureSlack) PostMessageContext(_ context.Context, channelID string, _ ...slack.MsgOption) (string, string, error) {
	c.mu.Lock()
	c.channels = append(c.channels, channelID)
	c.mu.Unlock()
	return "", "", nil
}
func (c *captureSlack) PostEphemeralContext(_ context.Context, _, _ string, _ ...slack.MsgOption) (string, error) {
	return "", nil
}
func (c *captureSlack) UpdateMessageContext(_ context.Context, _, _ string, _ ...slack.MsgOption) (string, string, string, error) {
	return "", "", "", nil
}
func (c *captureSlack) OpenViewContext(_ context.Context, _ string, _ slack.ModalViewRequest) (*slack.ViewResponse, error) {
	return nil, nil
}

func (c *captureSlack) hasMessageTo(channel string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range c.channels {
		if ch == channel {
			return true
		}
	}
	return false
}

// nopAudit discards audit events.
type nopAudit struct{}

func (nopAudit) Log(_ context.Context, _ audit.AuditEvent) error { return nil }

// captureAudit records audit events for assertions.
type captureAudit struct {
	mu     sync.Mutex
	events []audit.AuditEvent
}

func (c *captureAudit) Log(_ context.Context, e audit.AuditEvent) error {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
	return nil
}

func (c *captureAudit) hasEvent(eventType string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.EventType == eventType {
			return true
		}
	}
	return false
}

// trackingGH wraps stubGH and tracks which methods were called.
type trackingGH struct {
	stubGH
	mu    sync.Mutex
	calls []string
}

func (t *trackingGH) record(name string) {
	t.mu.Lock()
	t.calls = append(t.calls, name)
	t.mu.Unlock()
}

func (t *trackingGH) called(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.calls {
		if c == name {
			return true
		}
	}
	return false
}

func (t *trackingGH) CommentApproved(ctx context.Context, pr int, approver string) error {
	t.record("CommentApproved")
	return t.stubGH.CommentApproved(ctx, pr, approver)
}
func (t *trackingGH) CommentRejected(ctx context.Context, pr int, approver, reason string) error {
	t.record("CommentRejected")
	return t.stubGH.CommentRejected(ctx, pr, approver, reason)
}
func (t *trackingGH) CommentCancelled(ctx context.Context, pr int, requester string) error {
	t.record("CommentCancelled")
	return t.stubGH.CommentCancelled(ctx, pr, requester)
}
func (t *trackingGH) CommentRequested(ctx context.Context, pr int, requester, app, tag, reason string) error {
	t.record("CommentRequested")
	return t.stubGH.CommentRequested(ctx, pr, requester, app, tag, reason)
}
func (t *trackingGH) RemoveLabel(ctx context.Context, pr int, label string) error {
	t.record("RemoveLabel")
	return t.stubGH.RemoveLabel(ctx, pr, label)
}
func (t *trackingGH) AddLabels(ctx context.Context, pr int, labels []string) error {
	t.record("AddLabels")
	return t.stubGH.AddLabels(ctx, pr, labels)
}
func (t *trackingGH) CommentNoOp(ctx context.Context, pr int, app, tag string) error {
	t.record("CommentNoOp")
	return t.stubGH.CommentNoOp(ctx, pr, app, tag)
}
func (t *trackingGH) CommentAutoDeployFailed(ctx context.Context, pr int, reason error) error {
	t.record("CommentAutoDeployFailed")
	return t.stubGH.CommentAutoDeployFailed(ctx, pr, reason)
}
func (t *trackingGH) ClosePR(ctx context.Context, pr int) error {
	t.record("ClosePR")
	return t.stubGH.ClosePR(ctx, pr)
}
func (t *trackingGH) MergePR(ctx context.Context, pr int, method string) error {
	t.record("MergePR")
	return t.stubGH.MergePR(ctx, pr, method)
}

// --- test harness helpers ---

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	// Construct a Store using its exported constructor with the miniredis addr.
	s := store.New(mr.Addr(), "")
	_ = rdb // rdb only used to verify; the store creates its own client
	return s
}

func newTestBot(t *testing.T, gh githubClient, sl *captureSlack, st *store.Store) *Bot {
	t.Helper()
	cfg := &config.Config{
		Slack:      config.SlackConfig{DeployChannel: "C_DEPLOY"},
		Deployment: config.DeploymentConfig{MergeMethod: "squash", LockTTL: "5m", StaleDuration: "2h"},
		Apps: []config.AppConfig{
			{App: "myapp", Environment: "prod", KustomizePath: "apps/myapp/kustomization.yaml", TagPattern: ".*"},
		},
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

// seedPendingDeploy stores a deploy record in Redis and acquires the lock,
// simulating what handleDeploySubmit would have done.
func seedPendingDeploy(t *testing.T, st *store.Store, prNumber int) {
	t.Helper()
	ctx := context.Background()
	d := &store.PendingDeploy{
		App:         "myapp-prod",
		Environment: "prod",
		Tag:         "v2.0.0",
		PRNumber:    prNumber,
		PRURL:       "https://github.com/org/repo/pull/1",
		Requester:   "gh-user",
		RequesterID: "U_REQUESTER",
		ApproverID:  "U_APPROVER",
		Reason:      "test deploy",
		RequestedAt: time.Now(),
		ExpiresAt:   time.Now().Add(2 * time.Hour),
		State:       store.StatePending,
	}
	if err := st.Set(ctx, d, 2*time.Hour); err != nil {
		t.Fatalf("seed pending deploy: %v", err)
	}
	_, _ = st.AcquireLock(ctx, "prod", "myapp-prod", "U_REQUESTER", 5*time.Minute)
}

// approveAction returns the BlockAction for an approve button press on PR 1.
func approveAction() *slack.BlockAction {
	return &slack.BlockAction{ActionID: ActionApprove, Value: "1"}
}

// approveCallback returns a minimal InteractionCallback for an approve event.
func approveCallback() slack.InteractionCallback {
	return slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{approveAction()},
		},
		User:    slack.User{ID: "U_APPROVER"},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C_CHANNEL"}}},
	}
}

// deploySubmitCallback returns a minimal InteractionCallback for a deploy modal submission.
func deploySubmitCallback() slack.InteractionCallback {
	return slack.InteractionCallback{
		Type: slack.InteractionTypeViewSubmission,
		View: slack.View{
			CallbackID: ModalCallbackDeploy,
			State: &slack.ViewState{
				Values: map[string]map[string]slack.BlockAction{
					BlockApp:       {ActionApp: {SelectedOption: slack.OptionBlockObject{Value: "myapp-prod"}}},
					BlockTag:       {ActionTag: {SelectedOption: slack.OptionBlockObject{}}},
					BlockTagManual: {ActionTagManual: {Value: "v2.0.0"}},
					BlockReason:    {ActionReason: {Value: "test"}},
					BlockApprover:  {ActionApprover: {SelectedUser: "U_APPROVER"}},
				},
			},
		},
		User: slack.User{ID: "U_REQUESTER", Name: "requester"},
	}
}

// --- tests ---

// TestHandleDeploySubmit_NoChange verifies that ErrNoChange from CreateDeployPR
// causes the bot to release the lock, notify the deploy channel, DM the
// requester, and not store a pending deploy.
func TestHandleDeploySubmit_NoChange(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	gh := &stubGH{
		createDeployPR: func(_ context.Context, _ githubpkg.CreatePRParams) (int, string, error) {
			return 0, "", githubpkg.ErrNoChange
		},
	}
	b := newTestBot(t, gh, sl, st)

	b.handleDeploySubmit(context.Background(), deploySubmitCallback())

	// Deploy channel must receive the no-op notice.
	if !sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected no-op notice posted to deploy channel")
	}

	// No pending deploy stored.
	deploys, _ := st.GetAll(context.Background())
	if len(deploys) != 0 {
		t.Errorf("expected no pending deploys, got %d", len(deploys))
	}

	// Lock must be released.
	locked, _ := st.IsLocked(context.Background(), "prod", "myapp-prod")
	if locked {
		t.Error("expected deploy lock released after no-op")
	}
}

// TestHandleApprove_ConflictAutoResolved verifies that when MergePR returns
// ErrMergeConflict the bot rebases and retries, completing normally when the
// retry succeeds.
func TestHandleApprove_ConflictAutoResolved(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}

	callCount := 0
	gh := &stubGH{
		mergePR: func(_ context.Context, _ int, _ string) error {
			callCount++
			if callCount == 1 {
				return githubpkg.ErrMergeConflict
			}
			return nil
		},
		rebaseDeployBranch: func(_ context.Context, _ githubpkg.CreatePRParams) error {
			return nil
		},
	}

	b := newTestBot(t, gh, sl, st)
	seedPendingDeploy(t, st, 1)

	b.handleApprove(context.Background(), approveCallback(), approveAction())

	// Deploy completes: store entry deleted, lock released.
	d, _ := st.Get(context.Background(), 1)
	if d != nil {
		t.Error("expected pending deploy deleted after successful merge")
	}
	locked, _ := st.IsLocked(context.Background(), "prod", "myapp-prod")
	if locked {
		t.Error("expected lock released after successful merge")
	}

	// Approval outcome posted to deploy channel.
	if !sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected approval outcome posted to deploy channel")
	}

	if callCount != 2 {
		t.Errorf("MergePR called %d times, want 2 (conflict + retry)", callCount)
	}
}

// TestHandleApprove_ConflictUnresolvable verifies that when both merge attempts
// fail the bot resets state to pending, keeps the lock, and notifies the
// approver and deploy channel.
func TestHandleApprove_ConflictUnresolvable(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}

	gh := &stubGH{
		mergePR: func(_ context.Context, _ int, _ string) error {
			return githubpkg.ErrMergeConflict
		},
		rebaseDeployBranch: func(_ context.Context, _ githubpkg.CreatePRParams) error {
			return nil
		},
	}

	b := newTestBot(t, gh, sl, st)
	seedPendingDeploy(t, st, 1)

	b.handleApprove(context.Background(), approveCallback(), approveAction())

	// State reset to pending; PR left open.
	d, _ := st.Get(context.Background(), 1)
	if d == nil {
		t.Fatal("expected pending deploy to remain after unresolvable conflict")
	}
	if d.State != store.StatePending {
		t.Errorf("state = %q, want %q", d.State, store.StatePending)
	}

	// Lock kept: deploy still in flight.
	locked, _ := st.IsLocked(context.Background(), "prod", "myapp-prod")
	if !locked {
		t.Error("expected deploy lock to remain held after unresolvable conflict")
	}

	// Deploy channel notified.
	if !sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected conflict failure notice posted to deploy channel")
	}
}

// TestHandleApprove_ConflictRebaseIsNoOp verifies that when RebaseDeployBranch
// returns ErrNoChange the bot closes the PR and releases the lock.
func TestHandleApprove_ConflictRebaseIsNoOp(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}

	var prClosed bool
	gh := &stubGH{
		mergePR: func(_ context.Context, _ int, _ string) error {
			return githubpkg.ErrMergeConflict
		},
		rebaseDeployBranch: func(_ context.Context, _ githubpkg.CreatePRParams) error {
			return githubpkg.ErrNoChange
		},
		closePR: func(_ context.Context, _ int) error {
			prClosed = true
			return nil
		},
	}

	b := newTestBot(t, gh, sl, st)
	seedPendingDeploy(t, st, 1)

	b.handleApprove(context.Background(), approveCallback(), approveAction())

	if !prClosed {
		t.Error("expected PR closed when rebase reveals no-op")
	}
	d, _ := st.Get(context.Background(), 1)
	if d != nil {
		t.Error("expected pending deploy removed after no-op close")
	}
	locked, _ := st.IsLocked(context.Background(), "prod", "myapp-prod")
	if locked {
		t.Error("expected lock released after no-op close")
	}
	if !sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected no-op notice posted to deploy channel")
	}
}

// TestHandleEvent_DeploySubmit_NoChange verifies dispatch through HandleEvent.
func TestHandleEvent_DeploySubmit_NoChange(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	gh := &stubGH{
		createDeployPR: func(_ context.Context, _ githubpkg.CreatePRParams) (int, string, error) {
			return 0, "", githubpkg.ErrNoChange
		},
	}
	b := newTestBot(t, gh, sl, st)

	b.HandleEvent(context.Background(), socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: deploySubmitCallback(),
	})

	if !sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected no-op notice via HandleEvent dispatch")
	}
}

// TestHandleDeploySubmit_HappyPath verifies the full deploy request path:
// PR created, pending deploy stored, lock held, audit logged, Slack notified.
func TestHandleDeploySubmit_HappyPath(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	al := &captureAudit{}
	gh := &trackingGH{}

	b := &Bot{
		slack: sl, store: st, gh: gh,
		ecrCache: stubECR{}, validator: stubValidator{}, auditLog: al,
		metrics: metrics.New(prometheus.NewRegistry()),
		cfg: config.NewHolder(&config.Config{
			Slack:      config.SlackConfig{DeployChannel: "C_DEPLOY"},
			Deployment: config.DeploymentConfig{MergeMethod: "squash", LockTTL: "5m", StaleDuration: "2h"},
			Apps:       []config.AppConfig{{App: "myapp", Environment: "prod", KustomizePath: "apps/myapp/kustomization.yaml", TagPattern: ".*"}},
		}, ""),
		log: zap.NewNop(),
	}

	b.handleDeploySubmit(context.Background(), deploySubmitCallback())

	// Pending deploy stored.
	d, err := st.Get(context.Background(), 1)
	if err != nil || d == nil {
		t.Fatal("expected pending deploy stored")
	}
	if d.App != "myapp-prod" || d.Tag != "v2.0.0" || d.Environment != "prod" {
		t.Errorf("deploy = %+v", d)
	}

	// Lock held.
	locked, _ := st.IsLocked(context.Background(), "prod", "myapp-prod")
	if !locked {
		t.Error("expected deploy lock held")
	}

	// GitHub comment posted.
	if !gh.called("CommentRequested") {
		t.Error("expected CommentRequested called")
	}

	// Slack deploy channel notified.
	if !sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected deploy channel notified")
	}

	// Audit event logged.
	if !al.hasEvent(audit.EventRequested) {
		t.Error("expected audit event logged")
	}
}

// TestHandleApprove_HappyPath verifies the full approve path: PR merged,
// pending deploy deleted, lock released, audit logged, history pushed.
func TestHandleApprove_HappyPath(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	al := &captureAudit{}
	gh := &trackingGH{}

	b := &Bot{
		slack: sl, store: st, gh: gh,
		ecrCache: stubECR{}, validator: stubValidator{}, auditLog: al,
		metrics: metrics.New(prometheus.NewRegistry()),
		cfg: config.NewHolder(&config.Config{
			Slack:      config.SlackConfig{DeployChannel: "C_DEPLOY"},
			Deployment: config.DeploymentConfig{MergeMethod: "squash", LockTTL: "5m", StaleDuration: "2h"},
			Apps:       []config.AppConfig{{App: "myapp", Environment: "prod", KustomizePath: "apps/myapp/kustomization.yaml", TagPattern: ".*"}},
		}, ""),
		log: zap.NewNop(),
	}

	seedPendingDeploy(t, st, 1)

	b.handleApprove(context.Background(), approveCallback(), approveAction())

	// Pending deploy deleted.
	d, _ := st.Get(context.Background(), 1)
	if d != nil {
		t.Error("expected pending deploy deleted after merge")
	}

	// Lock released.
	locked, _ := st.IsLocked(context.Background(), "prod", "myapp-prod")
	if locked {
		t.Error("expected lock released after merge")
	}

	// GitHub side effects.
	if !gh.called("MergePR") {
		t.Error("expected MergePR called")
	}
	if !gh.called("CommentApproved") {
		t.Error("expected CommentApproved called")
	}
	if !gh.called("RemoveLabel") {
		t.Error("expected RemoveLabel called")
	}

	// Slack notified.
	if !sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected deploy channel notified")
	}

	// Audit event.
	if !al.hasEvent(audit.EventApproved) {
		t.Error("expected approved audit event")
	}

	// History entry.
	entries, _ := st.GetHistory(context.Background(), 10)
	if len(entries) == 0 {
		t.Error("expected history entry pushed")
	} else if entries[0].EventType != audit.EventApproved {
		t.Errorf("history event = %q, want %q", entries[0].EventType, audit.EventApproved)
	}
}

// TestHandleRejectSubmit_HappyPath verifies the full reject path: PR closed,
// pending deploy deleted, lock released, audit logged, history pushed.
func TestHandleRejectSubmit_HappyPath(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	al := &captureAudit{}
	gh := &trackingGH{}

	b := &Bot{
		slack: sl, store: st, gh: gh,
		ecrCache: stubECR{}, validator: stubValidator{}, auditLog: al,
		metrics: metrics.New(prometheus.NewRegistry()),
		cfg: config.NewHolder(&config.Config{
			Slack:      config.SlackConfig{DeployChannel: "C_DEPLOY"},
			Deployment: config.DeploymentConfig{MergeMethod: "squash", LockTTL: "5m", StaleDuration: "2h"},
			Apps:       []config.AppConfig{{App: "myapp", Environment: "prod", KustomizePath: "apps/myapp/kustomization.yaml", TagPattern: ".*"}},
		}, ""),
		log: zap.NewNop(),
	}

	seedPendingDeploy(t, st, 1)

	rejectCallback := slack.InteractionCallback{
		Type: slack.InteractionTypeViewSubmission,
		View: slack.View{
			CallbackID:      ModalCallbackReject,
			PrivateMetadata: "1",
			State: &slack.ViewState{
				Values: map[string]map[string]slack.BlockAction{
					BlockRejReason: {ActionRejReason: {Value: "not ready"}},
				},
			},
		},
		User: slack.User{ID: "U_APPROVER"},
	}

	b.handleRejectSubmit(context.Background(), rejectCallback)

	// Pending deploy deleted.
	d, _ := st.Get(context.Background(), 1)
	if d != nil {
		t.Error("expected pending deploy deleted after reject")
	}

	// Lock released.
	locked, _ := st.IsLocked(context.Background(), "prod", "myapp-prod")
	if locked {
		t.Error("expected lock released after reject")
	}

	// GitHub side effects.
	if !gh.called("CommentRejected") {
		t.Error("expected CommentRejected called")
	}
	if !gh.called("ClosePR") {
		t.Error("expected ClosePR called")
	}
	if !gh.called("RemoveLabel") {
		t.Error("expected RemoveLabel called")
	}

	// Slack notified.
	if !sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected deploy channel notified")
	}

	// Audit event.
	if !al.hasEvent(audit.EventRejected) {
		t.Error("expected rejected audit event")
	}

	// History entry.
	entries, _ := st.GetHistory(context.Background(), 10)
	if len(entries) == 0 {
		t.Error("expected history entry pushed")
	} else if entries[0].EventType != audit.EventRejected {
		t.Errorf("history event = %q, want %q", entries[0].EventType, audit.EventRejected)
	}
}

// TestHandleCancel_HappyPath verifies the full cancel path: PR closed,
// pending deploy deleted, lock released, audit logged, history pushed.
func TestHandleCancel_HappyPath(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	al := &captureAudit{}
	gh := &trackingGH{}

	b := &Bot{
		slack: sl, store: st, gh: gh,
		ecrCache: stubECR{}, validator: stubValidator{}, auditLog: al,
		metrics: metrics.New(prometheus.NewRegistry()),
		cfg: config.NewHolder(&config.Config{
			Slack:      config.SlackConfig{DeployChannel: "C_DEPLOY"},
			Deployment: config.DeploymentConfig{MergeMethod: "squash", LockTTL: "5m", StaleDuration: "2h"},
			Apps:       []config.AppConfig{{App: "myapp", Environment: "prod", KustomizePath: "apps/myapp/kustomization.yaml", TagPattern: ".*"}},
		}, ""),
		log: zap.NewNop(),
	}

	seedPendingDeploy(t, st, 1)

	cmd := slack.SlashCommand{
		UserID:   "U_REQUESTER",
		UserName: "requester",
	}

	b.handleCancel(context.Background(), cmd, "1")

	// Pending deploy deleted.
	d, _ := st.Get(context.Background(), 1)
	if d != nil {
		t.Error("expected pending deploy deleted after cancel")
	}

	// Lock released.
	locked, _ := st.IsLocked(context.Background(), "prod", "myapp-prod")
	if locked {
		t.Error("expected lock released after cancel")
	}

	// GitHub side effects.
	if !gh.called("CommentCancelled") {
		t.Error("expected CommentCancelled called")
	}
	if !gh.called("ClosePR") {
		t.Error("expected ClosePR called")
	}
	if !gh.called("RemoveLabel") {
		t.Error("expected RemoveLabel called")
	}

	// Slack notified.
	if !sl.hasMessageTo("C_DEPLOY") {
		t.Error("expected deploy channel notified")
	}

	// Audit event.
	if !al.hasEvent(audit.EventCancelled) {
		t.Error("expected cancelled audit event")
	}

	// History entry.
	entries, _ := st.GetHistory(context.Background(), 10)
	if len(entries) == 0 {
		t.Error("expected history entry pushed")
	} else if entries[0].EventType != audit.EventCancelled {
		t.Errorf("history event = %q, want %q", entries[0].EventType, audit.EventCancelled)
	}
}
