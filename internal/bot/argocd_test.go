package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/store"
)

// seedHistoryForArgoCD pushes a single approved history entry that the
// ArgoCD handler can correlate against via FindHistoryBySHA. Defaults
// match the shape a phase-1-enriched handleApprove writes: composite
// app/env name, non-empty gitops SHA, and a Slack handle for permalink
// resolution. Individual tests override any field they care about.
func seedHistoryForArgoCD(t *testing.T, st *store.Store, overrides ...func(*store.HistoryEntry)) *store.HistoryEntry {
	t.Helper()
	e := &store.HistoryEntry{
		EventType:       "approved",
		App:             "myapp",
		Environment:     "prod",
		Tag:             "v1.4.2",
		PRNumber:        1234,
		PRURL:           "https://github.com/org/gitops/pull/1234",
		RequesterID:     "U_REQUESTER",
		CompletedAt:     time.Now(), // fresh deploy — not late arrival
		GitopsCommitSHA: "abc123deadbeef",
		SlackChannel:    "C_DEPLOY",
		SlackMessageTS:  "1700000000.123456",
	}
	for _, f := range overrides {
		f(e)
	}
	if err := st.PushHistory(context.Background(), *e); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	return e
}

// resourcesJSON builds a json.RawMessage matching the shape the webhook
// template emits. Healthy entries in the slice are filtered out by the
// handler, so tests can freely mix healthy and unhealthy ones.
func resourcesJSON(t *testing.T, entries ...argocdResource) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal resources: %v", err)
	}
	return b
}

// TestArgoCDHandler_UnmatchedSHA_Drops verifies the contract for a
// notification that can't be correlated: the handler logs, posts
// nothing, and does not error. Without a history entry we don't know
// who deployed or what tag, so a "something broke somewhere" message
// would be strictly noise.
func TestArgoCDHandler_UnmatchedSHA_Drops(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	// Seed a history entry with a different SHA — lookup must miss.
	_ = seedHistoryForArgoCD(t, st, func(e *store.HistoryEntry) {
		e.GitopsCommitSHA = "some-other-sha"
	})

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-sync-failed",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: "abc123deadbeef",
	})

	if len(sl.posts) != 0 {
		t.Errorf("expected no Slack posts for unmatched SHA, got %d", len(sl.posts))
	}
}

// TestArgoCDHandler_SyncSucceeded_ThreadsUnderOriginalDeploy verifies
// the quiet happy-path: a threaded :white_check_mark: reply lands in
// the channel and ts recorded on the history entry, not a top-level
// post in whatever channel config currently points at.
func TestArgoCDHandler_SyncSucceeded_ThreadsUnderOriginalDeploy(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	entry := seedHistoryForArgoCD(t, st)

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-sync-succeeded",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: entry.GitopsCommitSHA,
		HealthStatus:    "Healthy",
		Phase:           "Succeeded",
	})

	if len(sl.posts) != 1 {
		t.Fatalf("expected 1 Slack post, got %d", len(sl.posts))
	}
	p := sl.posts[0]
	if p.Channel != entry.SlackChannel {
		t.Errorf("channel = %q, want %q (posted to original deploy channel)",
			p.Channel, entry.SlackChannel)
	}
	if p.ThreadTS != entry.SlackMessageTS {
		t.Errorf("thread_ts = %q, want %q (must thread under original deploy)",
			p.ThreadTS, entry.SlackMessageTS)
	}
	if !strings.Contains(p.Text, ":white_check_mark:") {
		t.Errorf("expected check-mark emoji in sync-succeeded reply, got: %s", p.Text)
	}
	if !strings.Contains(p.Text, "v1.4.2") {
		t.Errorf("expected tag in text, got: %s", p.Text)
	}
}

// TestArgoCDHandler_SyncSucceeded_NoHandleDrops verifies that a
// sync-succeeded for a deploy whose history entry has no Slack handle
// (ECR auto-deploy records a history entry but no approval-request
// message, so SlackChannel/SlackMessageTS are empty) is dropped
// rather than flattened into a standalone channel post. Sync-succeeded
// is the quiet path — we'd rather lose it than crowd the channel.
func TestArgoCDHandler_SyncSucceeded_NoHandleDrops(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	_ = seedHistoryForArgoCD(t, st, func(e *store.HistoryEntry) {
		e.SlackChannel = ""
		e.SlackMessageTS = ""
	})

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-sync-succeeded",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: "abc123deadbeef",
	})

	if len(sl.posts) != 0 {
		t.Errorf("expected no posts when sync-succeeded has no thread to reply in, got %d", len(sl.posts))
	}
}

// TestArgoCDHandler_SyncFailed_PostsTopLevelAlarming verifies the
// load-bearing failure-posting path: a fresh deploy getting a failed
// sync produces an UNTHREADED top-level message with siren emojis, an
// ALL-CAPS banner, a requester mention, the ArgoCD message, and the
// PR link. The permalink section exercises the captureSlack
// GetPermalinkContext stub.
func TestArgoCDHandler_SyncFailed_PostsTopLevelAlarming(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	entry := seedHistoryForArgoCD(t, st)

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-sync-failed",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: entry.GitopsCommitSHA,
		Phase:           "Failed",
		Message:         "Could not apply manifests",
	})

	if len(sl.posts) != 1 {
		t.Fatalf("expected 1 post, got %d", len(sl.posts))
	}
	p := sl.posts[0]
	if p.Channel != entry.SlackChannel {
		t.Errorf("channel = %q, want %q", p.Channel, entry.SlackChannel)
	}
	if p.ThreadTS != "" {
		t.Errorf("thread_ts = %q, want empty (failure must be top-level)", p.ThreadTS)
	}
	// Alarming framing checks:
	if !strings.Contains(p.Text, ":rotating_light:") {
		t.Error("expected siren emoji in failure message")
	}
	if !strings.Contains(p.Text, "DEPLOY FAILED") {
		t.Error("expected DEPLOY FAILED banner")
	}
	if !strings.Contains(p.Text, "<@U_REQUESTER>") {
		t.Error("expected requester ping in failure message")
	}
	if !strings.Contains(p.Text, "v1.4.2") {
		t.Errorf("expected tag in failure message: %s", p.Text)
	}
	if !strings.Contains(p.Text, "Could not apply manifests") {
		t.Errorf("expected ArgoCD message verbatim in body: %s", p.Text)
	}
	if !strings.Contains(p.Text, "Original deploy") {
		t.Error("expected Original deploy permalink anchor")
	}
	if !strings.Contains(p.Text, "PR #1234") {
		t.Error("expected PR link")
	}
	// And must NOT be framed as late arrival.
	if strings.Contains(p.Text, "more than 2 hours old") {
		t.Error("fresh deploy must not get late-arrival framing")
	}
}

// TestArgoCDHandler_HealthDegraded_RendersFailingResources verifies
// that the resources array in the payload is parsed, filtered to
// non-healthy entries, and rendered as a bullet list in the failure
// message — this is the "clearly communicate the issue in the
// channel" requirement from the planning discussion.
func TestArgoCDHandler_HealthDegraded_RendersFailingResources(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	entry := seedHistoryForArgoCD(t, st)
	resources := resourcesJSON(t,
		// Should render:
		argocdResource{
			Kind: "Deployment", Name: "myapp", Namespace: "default",
			SyncStatus: "Synced", HealthStatus: "Degraded",
			HealthMessage: "ReplicaSet has timed out progressing",
		},
		argocdResource{
			Kind: "Pod", Name: "myapp-xk9n2",
			SyncStatus: "Synced", HealthStatus: "Degraded",
			HealthMessage: "CrashLoopBackOff",
		},
		// Should be filtered out:
		argocdResource{
			Kind: "ConfigMap", Name: "myapp-config",
			SyncStatus: "Synced", // no health
		},
		argocdResource{
			Kind: "Service", Name: "myapp",
			SyncStatus: "Synced", HealthStatus: "Healthy",
		},
	)

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-health-degraded",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: entry.GitopsCommitSHA,
		HealthStatus:    "Degraded",
		Resources:       resources,
	})

	if len(sl.posts) != 1 {
		t.Fatalf("expected 1 post, got %d", len(sl.posts))
	}
	text := sl.posts[0].Text

	if !strings.Contains(text, "HEALTH DEGRADED") {
		t.Error("expected HEALTH DEGRADED banner")
	}
	if !strings.Contains(text, "Failing resources") {
		t.Error("expected failing-resources section")
	}
	// Failing resources rendered:
	if !strings.Contains(text, "`Deployment/myapp`") {
		t.Error("expected Deployment/myapp in failing list")
	}
	if !strings.Contains(text, "`Pod/myapp-xk9n2`") {
		t.Error("expected Pod/myapp-xk9n2 in failing list")
	}
	if !strings.Contains(text, "ReplicaSet has timed out progressing") {
		t.Error("expected failing-resource health message")
	}
	// Healthy/no-health resources filtered out:
	if strings.Contains(text, "ConfigMap/myapp-config") {
		t.Error("ConfigMap without health status should have been filtered out")
	}
	if strings.Contains(text, "Service/myapp") {
		t.Error("healthy Service should have been filtered out")
	}
}

// TestArgoCDHandler_LateArrival_Reframed verifies the "this deploy is
// old, a rollback may not be the right fix" framing kicks in when the
// history entry's CompletedAt is past lateArrivalThreshold. The
// message must NOT use the siren-heavy alarming framing, and must
// carry the "investigate before rolling back" note.
func TestArgoCDHandler_LateArrival_Reframed(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	entry := seedHistoryForArgoCD(t, st, func(e *store.HistoryEntry) {
		// Well past the 2h threshold.
		e.CompletedAt = time.Now().Add(-5 * time.Hour)
	})

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-health-degraded",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: entry.GitopsCommitSHA,
		HealthStatus:    "Degraded",
		Message:         "Pod OOMKilled",
	})

	if len(sl.posts) != 1 {
		t.Fatalf("expected 1 post, got %d", len(sl.posts))
	}
	text := sl.posts[0].Text

	if strings.Contains(text, ":rotating_light:") {
		t.Error("late arrival must not use siren framing")
	}
	if strings.Contains(text, "HEALTH DEGRADED") {
		t.Error("late arrival must not use ALL-CAPS banner")
	}
	if !strings.Contains(text, "previously-deployed") {
		t.Errorf("expected 'previously-deployed' framing, got: %s", text)
	}
	if !strings.Contains(text, "more than 2 hours old") {
		t.Error("expected late-arrival investigation note")
	}
	if !strings.Contains(text, "hours ago") {
		t.Error("expected human-readable age (e.g. '5 hours ago')")
	}
}

// --- unit tests for pure helpers (no Bot, no Slack) ---

func TestParseAndFilterResources(t *testing.T) {
	raw := resourcesJSON(t,
		argocdResource{Kind: "Deployment", Name: "bad", HealthStatus: "Degraded"},
		argocdResource{Kind: "Service", Name: "ok", HealthStatus: "Healthy"},
		argocdResource{Kind: "ConfigMap", Name: "no-health"},
		argocdResource{Kind: "Pod", Name: "missing", HealthStatus: "Missing"},
	)
	got := parseAndFilterResources(raw)
	if len(got) != 2 {
		t.Fatalf("expected 2 failing resources, got %d", len(got))
	}
	if got[0].Name != "bad" || got[1].Name != "missing" {
		t.Errorf("wrong filter result: %+v", got)
	}
}

func TestParseAndFilterResources_EmptyAndInvalid(t *testing.T) {
	if got := parseAndFilterResources(nil); got != nil {
		t.Errorf("nil input should yield nil, got %+v", got)
	}
	if got := parseAndFilterResources(json.RawMessage(`not json`)); got != nil {
		t.Errorf("invalid JSON should yield nil, got %+v", got)
	}
}

func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "moments ago"},
		{1 * time.Minute, "1 minute ago"},
		{15 * time.Minute, "15 minutes ago"},
		{1 * time.Hour, "1 hour ago"},
		{5 * time.Hour, "5 hours ago"},
		{2 * 24 * time.Hour, "2 days ago"},
	}
	for _, tc := range cases {
		if got := humanizeAge(tc.d); got != tc.want {
			t.Errorf("humanizeAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
