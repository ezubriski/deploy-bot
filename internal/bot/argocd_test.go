package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/slack-go/slack"

	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/store"
)

// seedHistoryForArgoCD pushes a single approved history entry that the
// ArgoCD handler can correlate against via FindHistoryBySHA. Defaults
// match the shape a phase-1-enriched handleApprove writes: the App
// field holds the composite "app-env" FullName (as produced by
// handleDeploySubmit's `appVal := appName + "-" + env`), a non-empty
// gitops SHA, and a Slack handle for permalink resolution. Individual
// tests override any field they care about.
func seedHistoryForArgoCD(t *testing.T, st *store.Store, overrides ...func(*store.HistoryEntry)) *store.HistoryEntry {
	t.Helper()
	e := &store.HistoryEntry{
		EventType:       "approved",
		App:             "myapp-prod",
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

// TestArgoCDHandler_HealthDegraded_SuppressesTransientRollout verifies
// the argocd-reconciler roll-up race workaround: on-health-degraded
// notifications for fresh deploys whose payload has no
// actually-degraded sub-resources are dropped silently, on the
// assumption that .status.health.status flipped to Degraded for a
// reconcile tick during a healthy RollingUpdate while per-resource
// health was stale. See isTransientRolloutDegraded for the fingerprint
// and the motivating homelab incident on 2026-04-11.
func TestArgoCDHandler_HealthDegraded_SuppressesTransientRollout(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	entry := seedHistoryForArgoCD(t, st) // fresh CompletedAt (now)

	// Payload shape the reconciler produced in the real incident:
	// app-level health Degraded, per-resource health uniformly empty.
	resources := resourcesJSON(t,
		argocdResource{Kind: "Service", Name: "nginx", Namespace: "dev", SyncStatus: "Synced"},
		argocdResource{Kind: "Deployment", Name: "nginx", Namespace: "dev", SyncStatus: "OutOfSync"},
	)

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-health-degraded",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: entry.GitopsCommitSHA,
		HealthStatus:    "Degraded",
		Message:         "successfully synced (all tasks run)",
		Resources:       resources,
	})

	if len(sl.posts) != 0 {
		t.Errorf("expected transient rollout to be suppressed (0 posts), got %d: %+v", len(sl.posts), sl.posts)
	}
	if got := argocdCounter(t, b, "on-health-degraded", metrics.ArgoCDResultTransientRolloutSkipped); got != 1 {
		t.Errorf("expected transient_rollout_skipped=1, got %v", got)
	}
	if got := argocdCounter(t, b, "on-health-degraded", metrics.ArgoCDResultMatched); got != 0 {
		t.Errorf("expected matched=0 (not posted), got %v", got)
	}
}

// TestArgoCDHandler_HealthDegraded_PostsWhenResourcesActuallyDegraded
// verifies the symmetric case to the suppress test above: a fresh
// deploy with at least one *genuinely* degraded resource in the
// payload must still post the alarming alert. Filters by-resource
// health — argocd's Lua health check populates healthMessage on real
// Deployment failures (e.g., "ReplicaSet has timed out progressing"),
// and the presence of any such resource bypasses the transient gate.
func TestArgoCDHandler_HealthDegraded_PostsWhenResourcesActuallyDegraded(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	entry := seedHistoryForArgoCD(t, st) // fresh

	resources := resourcesJSON(t,
		argocdResource{Kind: "Service", Name: "nginx", SyncStatus: "Synced", HealthStatus: "Healthy"},
		argocdResource{
			Kind:          "Deployment",
			Name:          "nginx",
			SyncStatus:    "Synced",
			HealthStatus:  "Degraded",
			HealthMessage: "ReplicaSet 'nginx-7d4' has timed out progressing",
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
		t.Fatalf("expected 1 post (real degraded), got %d", len(sl.posts))
	}
	if got := argocdCounter(t, b, "on-health-degraded", metrics.ArgoCDResultMatched); got != 1 {
		t.Errorf("expected matched=1, got %v", got)
	}
	if got := argocdCounter(t, b, "on-health-degraded", metrics.ArgoCDResultTransientRolloutSkipped); got != 0 {
		t.Errorf("expected transient_rollout_skipped=0, got %v", got)
	}
}

// TestArgoCDHandler_HealthDegraded_PostsWhenOldDeployEvenWithEmptyResources
// verifies the time gate on the transient-suppression path: a deploy
// older than transientGraceWindow must post on empty-resources
// degraded events. Rationale — a deploy that has been healthy for 10+
// minutes and is NOW reporting Degraded with no per-resource detail is
// much more likely to be a real bug (argocd wedged, resource health
// reconciler crashed) than a transient rollout artifact, and
// suppressing it would swallow a signal worth investigating. The
// existing late-arrival reframing (>2h) still applies separately when
// the deploy is very old.
func TestArgoCDHandler_HealthDegraded_PostsWhenOldDeployEvenWithEmptyResources(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	// Deploy older than transientGraceWindow (10m) but younger than
	// lateArrivalThreshold (2h). Hits the alarming-framing branch.
	entry := seedHistoryForArgoCD(t, st, func(e *store.HistoryEntry) {
		e.CompletedAt = time.Now().Add(-30 * time.Minute)
	})

	resources := resourcesJSON(t,
		argocdResource{Kind: "Service", Name: "nginx", SyncStatus: "Synced"},
		argocdResource{Kind: "Deployment", Name: "nginx", SyncStatus: "Synced"},
	)

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-health-degraded",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: entry.GitopsCommitSHA,
		HealthStatus:    "Degraded",
		Resources:       resources,
	})

	if len(sl.posts) != 1 {
		t.Fatalf("expected 1 post (old deploy), got %d", len(sl.posts))
	}
	if got := argocdCounter(t, b, "on-health-degraded", metrics.ArgoCDResultMatched); got != 1 {
		t.Errorf("expected matched=1, got %v", got)
	}
	if got := argocdCounter(t, b, "on-health-degraded", metrics.ArgoCDResultTransientRolloutSkipped); got != 0 {
		t.Errorf("expected transient_rollout_skipped=0, got %v", got)
	}
}

// TestIsTransientRolloutDegraded_Unit exercises the helper directly
// across its truth table so the semantic gates are locked in even
// without a full Bot + Store + Slack scaffold.
func TestIsTransientRolloutDegraded_Unit(t *testing.T) {
	fresh := time.Now()
	old := time.Now().Add(-30 * time.Minute)

	emptyResources := resourcesJSON(t,
		argocdResource{Kind: "Deployment", Name: "nginx"},
	)
	degradedResources := resourcesJSON(t,
		argocdResource{Kind: "Deployment", Name: "nginx", HealthStatus: "Degraded", HealthMessage: "timed out"},
	)

	tests := []struct {
		name  string
		entry *store.HistoryEntry
		evt   queue.ArgoCDNotificationEvent
		want  bool
	}{
		{
			name:  "nil entry — conservative pass-through",
			entry: nil,
			evt:   queue.ArgoCDNotificationEvent{Resources: emptyResources},
			want:  false,
		},
		{
			name:  "zero CompletedAt — conservative pass-through",
			entry: &store.HistoryEntry{},
			evt:   queue.ArgoCDNotificationEvent{Resources: emptyResources},
			want:  false,
		},
		{
			name:  "fresh deploy + empty resources — transient",
			entry: &store.HistoryEntry{CompletedAt: fresh},
			evt:   queue.ArgoCDNotificationEvent{Resources: emptyResources},
			want:  true,
		},
		{
			name:  "fresh deploy + degraded resource — real failure",
			entry: &store.HistoryEntry{CompletedAt: fresh},
			evt:   queue.ArgoCDNotificationEvent{Resources: degradedResources},
			want:  false,
		},
		{
			name:  "old deploy + empty resources — outside time gate",
			entry: &store.HistoryEntry{CompletedAt: old},
			evt:   queue.ArgoCDNotificationEvent{Resources: emptyResources},
			want:  false,
		},
		{
			name:  "old deploy + degraded resource — real failure",
			entry: &store.HistoryEntry{CompletedAt: old},
			evt:   queue.ArgoCDNotificationEvent{Resources: degradedResources},
			want:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientRolloutDegraded(tc.evt, tc.entry); got != tc.want {
				t.Errorf("isTransientRolloutDegraded: got %v, want %v", got, tc.want)
			}
		})
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

// argocdCounter is a small helper that reads the current value of the
// argocd notifications counter for a (trigger, result) pair on the bot's
// metrics registry. Used by the counter-assertion tests below.
func argocdCounter(t *testing.T, b *Bot, trigger, result string) float64 {
	t.Helper()
	return testutil.ToFloat64(b.metrics.ArgoCDNotificationsTotal.WithLabelValues(trigger, result))
}

// TestArgoCDHandler_Counter_Matched_OnFailure verifies the matched label
// fires on the failure path. This is the load-bearing success case for
// the observability feature: if this counter doesn't move when phase 3
// is working as intended, monitoring the feature is broken.
func TestArgoCDHandler_Counter_Matched_OnFailure(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	entry := seedHistoryForArgoCD(t, st)

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-sync-failed",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: entry.GitopsCommitSHA,
	})

	if got := argocdCounter(t, b, "on-sync-failed", metrics.ArgoCDResultMatched); got != 1 {
		t.Errorf("matched counter = %v, want 1", got)
	}
	// And no other label combinations were touched.
	if got := argocdCounter(t, b, "on-sync-failed", metrics.ArgoCDResultUnmatched); got != 0 {
		t.Errorf("unmatched counter = %v, want 0", got)
	}
}

// TestArgoCDHandler_Counter_Matched_OnSuccess verifies the matched label
// also fires for the quiet sync-succeeded happy path when a slack handle
// is available.
func TestArgoCDHandler_Counter_Matched_OnSuccess(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	entry := seedHistoryForArgoCD(t, st)

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-sync-succeeded",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: entry.GitopsCommitSHA,
	})

	if got := argocdCounter(t, b, "on-sync-succeeded", metrics.ArgoCDResultMatched); got != 1 {
		t.Errorf("matched counter = %v, want 1", got)
	}
}

// TestArgoCDHandler_Counter_NoHandleSkipped verifies that sync-succeeded
// arriving for a history entry with no slack handle (ECR auto-deploy
// shape) increments the no_handle_skipped counter rather than matched —
// the notification was correlated but we deliberately chose not to post.
func TestArgoCDHandler_Counter_NoHandleSkipped(t *testing.T) {
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

	if got := argocdCounter(t, b, "on-sync-succeeded", metrics.ArgoCDResultNoHandleSkipped); got != 1 {
		t.Errorf("no_handle_skipped counter = %v, want 1", got)
	}
	if got := argocdCounter(t, b, "on-sync-succeeded", metrics.ArgoCDResultMatched); got != 0 {
		t.Errorf("matched counter = %v, want 0 (the skipped case must not increment matched)", got)
	}
}

// TestArgoCDHandler_Counter_Unmatched verifies the unmatched label fires
// when FindHistoryBySHA returns (nil, nil). Operators can alert on a
// sustained non-zero rate to catch deploys that somehow bypass deploy-bot.
func TestArgoCDHandler_Counter_Unmatched(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	// No history seeded — every lookup misses.
	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-health-degraded",
		ArgoCDApp:       "unknown-app",
		GitopsCommitSHA: "never-seen-sha",
	})

	if got := argocdCounter(t, b, "on-health-degraded", metrics.ArgoCDResultUnmatched); got != 1 {
		t.Errorf("unmatched counter = %v, want 1", got)
	}
}

// TestArgoCDHandler_Counter_UnhandledTrigger verifies that a trigger name
// the bot doesn't know about increments the unhandled_trigger label (and
// is collapsed to the "other" trigger bucket for cardinality safety).
// This is the signal operators should use to catch "upstream added a new
// trigger we haven't wired yet."
func TestArgoCDHandler_Counter_UnhandledTrigger(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	entry := seedHistoryForArgoCD(t, st)

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-something-new-from-upstream",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: entry.GitopsCommitSHA,
	})

	// Trigger label collapses to "other" — bounded cardinality.
	if got := argocdCounter(t, b, "other", metrics.ArgoCDResultUnhandledTrigger); got != 1 {
		t.Errorf("unhandled_trigger counter = %v, want 1", got)
	}
	// And no Slack post happened.
	if len(sl.posts) != 0 {
		t.Errorf("expected no Slack posts for unhandled trigger, got %d", len(sl.posts))
	}
}

// --- phase 4: rollback prompt tests ---

// TestArgoCDHandler_SyncFailed_PostsRollbackPrompt verifies that a fresh
// failure with a prior approved deploy produces TWO top-level posts: the
// alarming status message AND a separate rollback prompt carrying Roll
// back / Dismiss buttons with a JSON payload referencing the previous
// known-good tag.
func TestArgoCDHandler_SyncFailed_PostsRollbackPrompt(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	// Prior known-good deploy: older, same app/env, approved. Note
	// App carries the composite FullName to match what handleApprove
	// writes in production — findPreviousApprovedBefore matches on
	// exact string equality so a mismatched shape would silently
	// suppress the prompt.
	if err := st.PushHistory(context.Background(), store.HistoryEntry{
		EventType:       "approved",
		App:             "myapp-prod",
		Environment:     "prod",
		Tag:             "v1.3.9",
		PRNumber:        1200,
		PRURL:           "https://github.com/org/gitops/pull/1200",
		RequesterID:     "U_PRIOR",
		CompletedAt:     time.Now().Add(-1 * time.Hour),
		GitopsCommitSHA: "prior-sha",
	}); err != nil {
		t.Fatalf("seed prior history: %v", err)
	}
	// Failing deploy: newer, the one the notification correlates to.
	entry := seedHistoryForArgoCD(t, st)

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-sync-failed",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: entry.GitopsCommitSHA,
		Message:         "boom",
	})

	if len(sl.posts) != 2 {
		t.Fatalf("expected 2 posts (status + rollback prompt), got %d", len(sl.posts))
	}
	status := sl.posts[0]
	prompt := sl.posts[1]

	if !strings.Contains(status.Text, "DEPLOY FAILED") {
		t.Errorf("first post should be the alarming status, got text: %s", status.Text)
	}
	// The prompt message is rendered via blocks; captureSlack records
	// the blocks JSON, so assertions hit that.
	if prompt.Blocks == "" {
		t.Fatal("rollback prompt should be posted as blocks")
	}
	if !strings.Contains(prompt.Blocks, "Suggested action") {
		t.Errorf("prompt blocks missing suggested-action text: %s", prompt.Blocks)
	}
	if !strings.Contains(prompt.Blocks, "v1.3.9") {
		t.Errorf("prompt blocks missing rollback target tag v1.3.9: %s", prompt.Blocks)
	}
	if !strings.Contains(prompt.Blocks, ActionArgoCDRollback) {
		t.Error("prompt blocks missing Roll back action id")
	}
	if !strings.Contains(prompt.Blocks, ActionArgoCDDismiss) {
		t.Error("prompt blocks missing Dismiss action id")
	}
	// Both buttons must be top-level, not threaded.
	if prompt.ThreadTS != "" {
		t.Errorf("rollback prompt must be top-level, got thread_ts=%q", prompt.ThreadTS)
	}
}

// TestArgoCDHandler_SyncFailed_NoPriorDeploy_NoPrompt verifies that the
// prompt is suppressed when there's no earlier approved deploy to roll
// back to — a first-ever deploy that fails still surfaces the alarming
// status message, but we don't post a [Roll back] button that can't
// resolve its target.
func TestArgoCDHandler_SyncFailed_NoPriorDeploy_NoPrompt(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	entry := seedHistoryForArgoCD(t, st)

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-sync-failed",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: entry.GitopsCommitSHA,
	})

	if len(sl.posts) != 1 {
		t.Fatalf("expected 1 post (status only), got %d", len(sl.posts))
	}
	if !strings.Contains(sl.posts[0].Text, "DEPLOY FAILED") {
		t.Errorf("expected alarming status without a rollback prompt, got: %s", sl.posts[0].Text)
	}
}

// TestArgoCDHandler_LateArrival_NoRollbackPrompt verifies that even
// when a prior known-good deploy exists, late-arriving failures
// (deploy > lateArrivalThreshold) suppress the rollback prompt. A
// hours-old deploy is rarely the right thing to roll back; the
// calmer status message still posts and points the on-call at
// investigation.
func TestArgoCDHandler_LateArrival_NoRollbackPrompt(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	// Prior approved deploy so findPreviousApprovedBefore would succeed.
	if err := st.PushHistory(context.Background(), store.HistoryEntry{
		EventType:       "approved",
		App:             "myapp-prod",
		Environment:     "prod",
		Tag:             "v1.3.9",
		CompletedAt:     time.Now().Add(-10 * time.Hour),
		GitopsCommitSHA: "prior-sha",
	}); err != nil {
		t.Fatalf("seed prior history: %v", err)
	}
	// Failing deploy is 5h old — past lateArrivalThreshold.
	entry := seedHistoryForArgoCD(t, st, func(e *store.HistoryEntry) {
		e.CompletedAt = time.Now().Add(-5 * time.Hour)
	})

	b.handleArgoCDNotification(context.Background(), queue.ArgoCDNotificationEvent{
		Trigger:         "on-health-degraded",
		ArgoCDApp:       "myapp-prod",
		GitopsCommitSHA: entry.GitopsCommitSHA,
	})

	if len(sl.posts) != 1 {
		t.Fatalf("expected 1 post (late-arrival status only), got %d", len(sl.posts))
	}
	if !strings.Contains(sl.posts[0].Text, "more than 2 hours old") {
		t.Errorf("expected late-arrival framing on the status message, got: %s", sl.posts[0].Text)
	}
}

func TestFindPreviousApprovedBefore_SkipsNewerAndRejected(t *testing.T) {
	now := time.Now()
	entries := []store.HistoryEntry{
		// newest first — a newer approved entry that must be ignored
		{App: "myapp-prod", Tag: "v2.0", EventType: "approved", CompletedAt: now.Add(1 * time.Hour)},
		// the failing entry itself (at `before`)
		{App: "myapp-prod", Tag: "v1.9", EventType: "approved", CompletedAt: now},
		// rejected entry must be skipped
		{App: "myapp-prod", Tag: "v1.8", EventType: "rejected", CompletedAt: now.Add(-30 * time.Minute)},
		// the correct answer
		{App: "myapp-prod", Tag: "v1.7", EventType: "approved", CompletedAt: now.Add(-1 * time.Hour)},
		// unrelated app
		{App: "other-prod", Tag: "v9.9", EventType: "approved", CompletedAt: now.Add(-2 * time.Hour)},
	}
	got, ok := findPreviousApprovedBefore(entries, "myapp-prod", now)
	if !ok {
		t.Fatal("expected a match")
	}
	if got.Tag != "v1.7" {
		t.Errorf("tag = %q, want v1.7", got.Tag)
	}
}

func TestFindPreviousApprovedBefore_NoMatch(t *testing.T) {
	now := time.Now()
	entries := []store.HistoryEntry{
		{App: "myapp-prod", Tag: "v1.9", EventType: "approved", CompletedAt: now},
	}
	if _, ok := findPreviousApprovedBefore(entries, "myapp-prod", now); ok {
		t.Error("expected no match when the only entry is the failing one")
	}
}

func TestArgoCDRollbackPayload_RoundTrip(t *testing.T) {
	p := argocdRollbackPayload{
		App:         "myapp-prod",
		Environment: "prod",
		FailingTag:  "v2.0.0+build.1",
		RollbackTag: "v1.9.9",
	}
	raw := p.marshal()
	got, err := parseArgoCDRollbackPayload(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != p {
		t.Errorf("round-trip mismatch: %+v != %+v", got, p)
	}
}

// TestHandleArgoCDDismissClick_ReplacesButtons verifies the happy path
// for the [Dismiss] button: an authorized clicker causes the prompt
// message to be updated in place so the buttons are replaced with a
// "Dismissed by" context line. The underlying failure status message
// is unaffected.
func TestHandleArgoCDDismissClick_ReplacesButtons(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	payload := argocdRollbackPayload{
		App: "myapp-prod", Environment: "prod",
		FailingTag: "v2.0.0", RollbackTag: "v1.9.0",
	}.marshal()

	cb := slack.InteractionCallback{
		Type:    slack.InteractionTypeBlockActions,
		User:    slack.User{ID: "U_APPROVER"},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C_DEPLOY"}}},
		Message: slack.Message{Msg: slack.Msg{Timestamp: "1700000000.222222"}},
	}
	b.handleArgoCDDismissClick(context.Background(), cb, &slack.BlockAction{
		ActionID: ActionArgoCDDismiss, Value: payload,
	})

	if len(sl.updates) != 1 {
		t.Fatalf("expected 1 UpdateMessageContext call, got %d", len(sl.updates))
	}
	up := sl.updates[0]
	if up.Channel != "C_DEPLOY" {
		t.Errorf("update channel = %q, want C_DEPLOY", up.Channel)
	}
	if !strings.Contains(up.Blocks, "Dismissed") {
		t.Errorf("updated blocks missing Dismissed marker: %s", up.Blocks)
	}
	if !strings.Contains(up.Blocks, "U_APPROVER") {
		t.Errorf("updated blocks missing dismissing user mention: %s", up.Blocks)
	}
	// Buttons must be gone from the updated message.
	if strings.Contains(up.Blocks, ActionArgoCDRollback) {
		t.Error("updated blocks still contain Roll back action id")
	}
	if strings.Contains(up.Blocks, ActionArgoCDDismiss) {
		t.Error("updated blocks still contain Dismiss action id")
	}
}

// TestHandleArgoCDRollbackClick_OpensDeployModal verifies the [Roll
// back] click path opens the deploy modal in rollback mode via
// OpenViewContext. The captureSlack stub is a sink for OpenViewContext,
// so this test just asserts the call happened without erroring —
// deeper modal-content assertions live in buildDeployModal tests.
func TestHandleArgoCDRollbackClick_OpensDeployModal(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	payload := argocdRollbackPayload{
		App: "myapp-prod", Environment: "prod",
		FailingTag: "v2.0.0", RollbackTag: "v1.9.0",
	}.marshal()

	cb := slack.InteractionCallback{
		Type:      slack.InteractionTypeBlockActions,
		User:      slack.User{ID: "U_DEPLOYER"},
		TriggerID: "trigger-xyz",
		Channel:   slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C_DEPLOY"}}},
		Message:   slack.Message{Msg: slack.Msg{Timestamp: "1700000000.333333"}},
	}
	b.handleArgoCDRollbackClick(context.Background(), cb, &slack.BlockAction{
		ActionID: ActionArgoCDRollback, Value: payload,
	})

	// The captureSlack open-view stub returns nil — if we got here
	// without panicking or erroring into the ephemeral path, the
	// modal was opened. Double-check no ephemeral error was posted.
	if len(sl.posts) != 0 {
		t.Errorf("expected no status posts (modal open only), got %d: %+v", len(sl.posts), sl.posts)
	}
}

func TestHandleArgoCDRollbackClick_BadPayload_Ephemeral(t *testing.T) {
	st := newTestStore(t)
	sl := &captureSlack{}
	b := newTestBot(t, &stubGH{}, sl, st)

	cb := slack.InteractionCallback{
		Type:    slack.InteractionTypeBlockActions,
		User:    slack.User{ID: "U_DEPLOYER"},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C_DEPLOY"}}},
	}
	b.handleArgoCDRollbackClick(context.Background(), cb, &slack.BlockAction{
		ActionID: ActionArgoCDRollback, Value: "not json",
	})

	// No UpdateMessageContext or modal open for malformed payload.
	if len(sl.updates) != 0 {
		t.Errorf("malformed payload must not update prompt, got %d updates", len(sl.updates))
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
