package argocd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/buffer"
	"github.com/ezubriski/deploy-bot/internal/queue"
)

const testSecret = "this-is-a-test-secret-32-chars-long!!"

// newTestHandler builds a handler backed by miniredis with no metrics.
// Returns the handler, the redis client (for stream inspection), and a
// cleanup func registered via t.Cleanup.
func newTestHandler(t *testing.T) (*WebhookHandler, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	log := zap.NewNop()
	buf := buffer.New(buffer.DefaultSize, rdb, queue.StreamKeyArgoCD, log)
	processor := NewProcessor(rdb, buf, log)
	return NewWebhookHandler(processor, testSecret, nil, log), rdb
}

func validPayloadJSON(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(WebhookPayload{
		Trigger:            "on-sync-succeeded",
		ArgoCDApp:          "myapp-prod",
		Namespace:          "argocd",
		RepoURL:            "https://github.com/org/gitops",
		Revision:           "abc123",
		SyncResultRevision: "deadbeefcafe0001",
		SyncStatus:         "Synced",
		HealthStatus:       "Healthy",
		Phase:              "Succeeded",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

func postJSON(t *testing.T, h http.Handler, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/argocd", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhook_RejectsNonPost(t *testing.T) {
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/argocd", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestWebhook_RejectsMissingSecret(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postJSON(t, h, validPayloadJSON(t), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestWebhook_RejectsWrongSecret(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postJSON(t, h, validPayloadJSON(t), map[string]string{
		secretHeader: "not-the-real-secret-but-padded-out-",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestWebhook_RejectsMalformedJSON(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postJSON(t, h, []byte("{not json"), map[string]string{
		secretHeader: testSecret,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestWebhook_RejectsMissingRequiredFields(t *testing.T) {
	h, _ := newTestHandler(t)
	body, _ := json.Marshal(WebhookPayload{
		// trigger and argocdApp are intentionally empty
		SyncResultRevision: "abc",
	})
	rec := postJSON(t, h, body, map[string]string{
		secretHeader: testSecret,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestWebhook_RejectsOversizedBody(t *testing.T) {
	h, _ := newTestHandler(t)
	// Build a body larger than the 1 MiB limit. We pad an opaque field
	// with whitespace inside a syntactically valid JSON object so the
	// MaxBytesReader trips before json.Unmarshal can finish.
	pad := strings.Repeat("x", (1<<20)+1024)
	body := []byte(`{"trigger":"on-sync-succeeded","argocdApp":"myapp","syncResultRevision":"sha","message":"` + pad + `"}`)
	rec := postJSON(t, h, body, map[string]string{
		secretHeader: testSecret,
	})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestWebhook_AcceptsValidPayloadAndEnqueues(t *testing.T) {
	h, rdb := newTestHandler(t)
	rec := postJSON(t, h, validPayloadJSON(t), map[string]string{
		secretHeader: testSecret,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s want 200", rec.Code, rec.Body.String())
	}

	// Stream should have one message.
	msgs, err := rdb.XRange(context.Background(), queue.StreamKeyArgoCD, "-", "+").Result()
	if err != nil {
		t.Fatalf("xrange: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message in argocd stream, got %d", len(msgs))
	}
}

func TestWebhook_DedupesRepeatNotification(t *testing.T) {
	h, rdb := newTestHandler(t)
	body := validPayloadJSON(t)

	rec1 := postJSON(t, h, body, map[string]string{secretHeader: testSecret})
	if rec1.Code != http.StatusOK || !strings.Contains(rec1.Body.String(), `"enqueued":true`) {
		t.Fatalf("first post: status=%d body=%s", rec1.Code, rec1.Body.String())
	}

	rec2 := postJSON(t, h, body, map[string]string{secretHeader: testSecret})
	if rec2.Code != http.StatusOK {
		t.Fatalf("second post status = %d, want 200", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), `"deduped":true`) {
		t.Errorf("second post body = %s, expected deduped=true", rec2.Body.String())
	}
	if strings.Contains(rec2.Body.String(), `"enqueued":true`) {
		t.Errorf("second post body = %s, expected enqueued=false", rec2.Body.String())
	}

	// Stream still has only the first message.
	msgs, _ := rdb.XRange(context.Background(), queue.StreamKeyArgoCD, "-", "+").Result()
	if len(msgs) != 1 {
		t.Errorf("expected 1 message in stream after dedupe, got %d", len(msgs))
	}
}

func TestWebhook_DropsUnrecognizedTrigger(t *testing.T) {
	h, rdb := newTestHandler(t)
	body, _ := json.Marshal(WebhookPayload{
		Trigger:            "on-deployed", // not one of the four we subscribe to
		ArgoCDApp:          "myapp-prod",
		SyncResultRevision: "abc123",
	})
	rec := postJSON(t, h, body, map[string]string{secretHeader: testSecret})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (unknown triggers are dropped, not rejected)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"recognized":false`) {
		t.Errorf("body = %s, expected recognized=false", rec.Body.String())
	}
	msgs, _ := rdb.XRange(context.Background(), queue.StreamKeyArgoCD, "-", "+").Result()
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages for unrecognized trigger, got %d", len(msgs))
	}
}

func TestWebhook_AcceptsButDropsSyncRunning(t *testing.T) {
	h, rdb := newTestHandler(t)
	body, _ := json.Marshal(WebhookPayload{
		Trigger:            "on-sync-running",
		ArgoCDApp:          "myapp-prod",
		SyncResultRevision: "abc123",
	})
	rec := postJSON(t, h, body, map[string]string{secretHeader: testSecret})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"recognized":true`) {
		t.Errorf("body = %s, expected recognized=true", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"enqueued":true`) {
		t.Errorf("body = %s, expected enqueued=false (sync-running is currently dropped)", rec.Body.String())
	}
	msgs, _ := rdb.XRange(context.Background(), queue.StreamKeyArgoCD, "-", "+").Result()
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages for on-sync-running, got %d", len(msgs))
	}
}

// TestWebhook_QueueRoundTrip enqueues via the webhook, then decodes the
// resulting stream message through the queue package and verifies the
// fields land where the worker expects them. This is the cross-package
// contract that phase 3 will rely on.
func TestWebhook_QueueRoundTrip(t *testing.T) {
	h, rdb := newTestHandler(t)
	body, _ := json.Marshal(WebhookPayload{
		Trigger:            "on-health-degraded",
		ArgoCDApp:          "myapp-prod",
		Namespace:          "argocd",
		RepoURL:            "https://github.com/org/gitops",
		SyncResultRevision: "feedface",
		HealthStatus:       "Degraded",
		Message:            "ReplicaSet has timed out progressing",
	})
	rec := postJSON(t, h, body, map[string]string{secretHeader: testSecret})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	msgs, err := rdb.XRange(context.Background(), queue.StreamKeyArgoCD, "-", "+").Result()
	if err != nil || len(msgs) != 1 {
		t.Fatalf("xrange: msgs=%d err=%v", len(msgs), err)
	}

	// Decode via the public queue path the worker uses.
	got, err := decodeArgoCDFromStream(t, msgs[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Trigger != "on-health-degraded" {
		t.Errorf("trigger = %q", got.Trigger)
	}
	if got.GitopsCommitSHA != "feedface" {
		t.Errorf("sha = %q, want feedface", got.GitopsCommitSHA)
	}
	if got.HealthStatus != "Degraded" {
		t.Errorf("health = %q", got.HealthStatus)
	}
	if got.ArgoCDApp != "myapp-prod" {
		t.Errorf("app = %q", got.ArgoCDApp)
	}
	if got.Message == "" {
		t.Errorf("message lost in encode/decode")
	}
}

// decodeArgoCDFromStream is a thin test helper that mirrors what the
// queue worker does internally: pull the payload field, parse the
// envelope, and unmarshal the inner ArgoCDNotificationEvent.
func decodeArgoCDFromStream(t *testing.T, msg redis.XMessage) (queue.ArgoCDNotificationEvent, error) {
	t.Helper()
	raw, ok := msg.Values["payload"].(string)
	if !ok {
		return queue.ArgoCDNotificationEvent{}, errMissingPayload
	}
	var env struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return queue.ArgoCDNotificationEvent{}, err
	}
	if socketmode.EventType(env.Type) != queue.EventTypeArgoCDNotification {
		return queue.ArgoCDNotificationEvent{}, errWrongType
	}
	var argo queue.ArgoCDNotificationEvent
	if err := json.Unmarshal(env.Data, &argo); err != nil {
		return queue.ArgoCDNotificationEvent{}, err
	}
	return argo, nil
}

var (
	errMissingPayload = &decodeError{"missing payload"}
	errWrongType      = &decodeError{"wrong event type"}
)

type decodeError struct{ msg string }

func (e *decodeError) Error() string { return e.msg }
