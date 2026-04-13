package healthcheck

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap/zaptest"
)

// mockQuerier returns a fixed result on each call.
type mockQuerier struct {
	mu      sync.Mutex
	results []*QueryResult
	errs    []error
	calls   int
}

func (m *mockQuerier) Query(_ context.Context, _ string) (*QueryResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.calls
	if idx >= len(m.results) {
		idx = len(m.results) - 1
	}
	m.calls++
	return m.results[idx], m.errs[idx]
}

func (m *mockQuerier) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// mockSlack captures Slack API calls for verification.
type mockSlack struct {
	mu      sync.Mutex
	posts   []mockPost
	updates []mockUpdate
	nextTS  string
}

type mockPost struct {
	Channel string
}

type mockUpdate struct {
	Channel string
	TS      string
}

func (m *mockSlack) PostMessageContext(_ context.Context, channelID string, _ ...slack.MsgOption) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posts = append(m.posts, mockPost{Channel: channelID})
	ts := m.nextTS
	if ts == "" {
		ts = "1700000000.000001"
	}
	return channelID, ts, nil
}

func (m *mockSlack) PostEphemeralContext(_ context.Context, _ string, _ string, _ ...slack.MsgOption) (string, error) {
	return "", nil
}

func (m *mockSlack) UpdateMessageContext(_ context.Context, channelID, timestamp string, _ ...slack.MsgOption) (string, string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updates = append(m.updates, mockUpdate{Channel: channelID, TS: timestamp})
	return channelID, timestamp, "", nil
}

func (m *mockSlack) OpenViewContext(_ context.Context, _ string, _ slack.ModalViewRequest) (*slack.ViewResponse, error) {
	return nil, nil
}

func (m *mockSlack) UpdateViewContext(_ context.Context, _ slack.ModalViewRequest, _ string, _ string, _ string) (*slack.ViewResponse, error) {
	return nil, nil
}

func (m *mockSlack) GetPermalinkContext(_ context.Context, _ *slack.PermalinkParameters) (string, error) {
	return "", nil
}

func (m *mockSlack) postCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.posts)
}

func (m *mockSlack) updateCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.updates)
}

func TestMonitor_Run_SingleCheck_Healthy(t *testing.T) {
	q := &mockQuerier{
		results: []*QueryResult{{Value: 0.99, OK: true}},
		errs:    []error{nil},
	}
	sl := &mockSlack{nextTS: "1700000000.100"}
	log := zaptest.NewLogger(t)

	mon := NewMonitor(map[string]MetricsQuerier{"dynatrace": q}, sl, log)

	p := Params{
		App:           "myapp-dev",
		Environment:   "dev",
		Tag:           "v1.0.0",
		PRNumber:      42,
		Checks:        []Check{{Provider: "dynatrace", Name: "uptime", Query: "fetch metrics", Threshold: "> 0.95"}},
		PollInterval:  50 * time.Millisecond,
		PollDuration:  180 * time.Millisecond,
		SlackChannel:  "C_DEPLOY",
		SlackThreadTS: "1700000000.000000",
	}

	mon.Run(context.Background(), p)

	if q.callCount() == 0 {
		t.Error("expected at least one query call")
	}
	if sl.postCount() < 1 {
		t.Errorf("expected at least 1 Slack post, got %d", sl.postCount())
	}
	if sl.updateCount() == 0 {
		t.Error("expected at least one Slack status update")
	}
}

func TestMonitor_Run_SingleCheck_Unhealthy_PostsRollback(t *testing.T) {
	q := &mockQuerier{
		results: []*QueryResult{{Value: 0.5, OK: true}},
		errs:    []error{nil},
	}
	sl := &mockSlack{nextTS: "1700000000.100"}
	log := zaptest.NewLogger(t)

	mon := NewMonitor(map[string]MetricsQuerier{"dynatrace": q}, sl, log)

	p := Params{
		App:           "myapp-dev",
		Environment:   "dev",
		Tag:           "v1.0.0",
		PRNumber:      42,
		Checks:        []Check{{Provider: "dynatrace", Name: "uptime", Query: "fetch metrics", Threshold: "> 0.95"}},
		PollInterval:  50 * time.Millisecond,
		PollDuration:  180 * time.Millisecond,
		SlackChannel:  "C_DEPLOY",
		SlackThreadTS: "1700000000.000000",
	}

	mon.Run(context.Background(), p)

	// Should have the initial status post + rollback prompt post.
	if sl.postCount() < 2 {
		t.Errorf("expected at least 2 Slack posts (initial + rollback), got %d", sl.postCount())
	}
}

func TestMonitor_Run_MultipleChecks_AND_Logic(t *testing.T) {
	// One check healthy, one unhealthy — overall should be unhealthy.
	qHealthy := &mockQuerier{
		results: []*QueryResult{{Value: 0.99, OK: true}},
		errs:    []error{nil},
	}
	qUnhealthy := &mockQuerier{
		results: []*QueryResult{{Value: 600, OK: true}},
		errs:    []error{nil},
	}
	sl := &mockSlack{nextTS: "1700000000.100"}
	log := zaptest.NewLogger(t)

	providers := map[string]MetricsQuerier{
		"provider_a": qHealthy,
		"provider_b": qUnhealthy,
	}
	mon := NewMonitor(providers, sl, log)

	p := Params{
		App:         "myapp-prod",
		Environment: "prod",
		Tag:         "v2.0.0",
		PRNumber:    99,
		Checks: []Check{
			{Provider: "provider_a", Name: "uptime", Query: "q1", Threshold: "> 0.95"},
			{Provider: "provider_b", Name: "latency", Query: "q2", Threshold: "< 500"},
		},
		PollInterval:  50 * time.Millisecond,
		PollDuration:  180 * time.Millisecond,
		SlackChannel:  "C_DEPLOY",
		SlackThreadTS: "1700000000.000000",
	}

	mon.Run(context.Background(), p)

	// Both providers should have been called.
	if qHealthy.callCount() == 0 {
		t.Error("expected provider_a to be queried")
	}
	if qUnhealthy.callCount() == 0 {
		t.Error("expected provider_b to be queried")
	}

	// Should have rollback prompt because one check failed.
	if sl.postCount() < 2 {
		t.Errorf("expected at least 2 Slack posts (initial + rollback), got %d", sl.postCount())
	}
}

func TestMonitor_Run_MultipleChecks_AllHealthy(t *testing.T) {
	q1 := &mockQuerier{
		results: []*QueryResult{{Value: 0.99, OK: true}},
		errs:    []error{nil},
	}
	q2 := &mockQuerier{
		results: []*QueryResult{{Value: 100, OK: true}},
		errs:    []error{nil},
	}
	sl := &mockSlack{nextTS: "1700000000.100"}
	log := zaptest.NewLogger(t)

	providers := map[string]MetricsQuerier{
		"provider_a": q1,
		"provider_b": q2,
	}
	mon := NewMonitor(providers, sl, log)

	p := Params{
		App:         "myapp-prod",
		Environment: "prod",
		Tag:         "v2.0.0",
		PRNumber:    99,
		Checks: []Check{
			{Provider: "provider_a", Name: "uptime", Query: "q1", Threshold: "> 0.95"},
			{Provider: "provider_b", Name: "latency", Query: "q2", Threshold: "< 500"},
		},
		PollInterval:  50 * time.Millisecond,
		PollDuration:  180 * time.Millisecond,
		SlackChannel:  "C_DEPLOY",
		SlackThreadTS: "1700000000.000000",
	}

	mon.Run(context.Background(), p)

	// No rollback prompt — only the initial status post.
	if sl.postCount() != 1 {
		t.Errorf("expected exactly 1 Slack post (initial status), got %d", sl.postCount())
	}
}

func TestMonitor_Run_ContextCancelled(t *testing.T) {
	q := &mockQuerier{
		results: []*QueryResult{{Value: 0.99, OK: true}},
		errs:    []error{nil},
	}
	sl := &mockSlack{nextTS: "1700000000.100"}
	log := zaptest.NewLogger(t)

	mon := NewMonitor(map[string]MetricsQuerier{"dt": q}, sl, log)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := Params{
		App:          "myapp-dev",
		Environment:  "dev",
		Tag:          "v1.0.0",
		Checks:       []Check{{Provider: "dt", Name: "check", Query: "q", Threshold: "> 0"}},
		PollInterval: 50 * time.Millisecond,
		PollDuration: 5 * time.Second,
		SlackChannel: "C_DEPLOY",
	}

	// Should return quickly without panicking.
	mon.Run(ctx, p)
}

func TestMonitor_Run_MissingProvider(t *testing.T) {
	sl := &mockSlack{nextTS: "1700000000.100"}
	log := zaptest.NewLogger(t)

	// No providers registered.
	mon := NewMonitor(map[string]MetricsQuerier{}, sl, log)

	p := Params{
		App:           "myapp-dev",
		Environment:   "dev",
		Tag:           "v1.0.0",
		Checks:        []Check{{Provider: "nonexistent", Name: "check", Query: "q", Threshold: "> 0"}},
		PollInterval:  50 * time.Millisecond,
		PollDuration:  180 * time.Millisecond,
		SlackChannel:  "C_DEPLOY",
		SlackThreadTS: "1700000000.000000",
	}

	mon.Run(context.Background(), p)

	// Should post rollback since the check fails (provider missing).
	if sl.postCount() < 2 {
		t.Errorf("expected at least 2 Slack posts, got %d", sl.postCount())
	}
}
