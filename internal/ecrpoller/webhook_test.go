package ecrpoller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/config"
	"github.com/ezubriski/deploy-bot/internal/metrics"
	"github.com/ezubriski/deploy-bot/internal/queue"
	"github.com/ezubriski/deploy-bot/internal/store"
)

const testAPIKey = "test-api-key-that-is-at-least-32-chars-long"

func newTestWebhookHandler(t *testing.T, apps []config.AppConfig) (*WebhookHandler, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	st := store.New(mr.Addr(), "")
	cfg := &config.Config{Apps: apps}
	holder := config.NewHolder(cfg, "")

	poller := NewWithoutSQS(rdb, &stubBuffer{}, st, holder, zap.NewNop())
	m := metrics.New(prometheus.NewRegistry())
	h := NewWebhookHandler(poller, testAPIKey, m, zap.NewNop())
	return h, rdb
}

func validECRPushBody(repo, tag string) []byte {
	eb := EventBridgeEvent{
		Source:     "aws.ecr",
		DetailType: "ECR Image Action",
		Detail: ECRPushDetail{
			ActionType:     "PUSH",
			Result:         "SUCCESS",
			RepositoryName: repo,
			ImageTag:       tag,
		},
	}
	data, _ := json.Marshal(eb)
	return data
}

func TestWebhook_ValidRequest(t *testing.T) {
	apps := []config.AppConfig{{
		App:         "myapp",
		Environment: "dev",
		ECRRepo:     "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp",
	}}
	h, rdb := newTestWebhookHandler(t, apps)
	ctx := context.Background()

	// Init consumer group so we can read.
	w := queue.NewWorker(rdb, zap.NewNop())
	_ = w.Init(ctx)

	body := validECRPushBody("myapp", "v1.0.0")
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ecr", bytes.NewReader(body))
	req.Header.Set("x-api-key", testAPIKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify event was enqueued.
	msgs, err := rdb.XRange(ctx, queue.StreamKeyECR, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 enqueued message, got %d", len(msgs))
	}
}

func TestWebhook_MissingAPIKey(t *testing.T) {
	h, _ := newTestWebhookHandler(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ecr", bytes.NewReader(validECRPushBody("myapp", "v1")))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestWebhook_WrongAPIKey(t *testing.T) {
	h, _ := newTestWebhookHandler(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ecr", bytes.NewReader(validECRPushBody("myapp", "v1")))
	req.Header.Set("x-api-key", "wrong-key-that-is-also-long-enough")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestWebhook_WrongMethod(t *testing.T) {
	h, _ := newTestWebhookHandler(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/ecr", nil)
	req.Header.Set("x-api-key", testAPIKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestWebhook_MalformedJSON(t *testing.T) {
	h, _ := newTestWebhookHandler(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ecr", bytes.NewReader([]byte("not json")))
	req.Header.Set("x-api-key", testAPIKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestWebhook_NonPushEvent(t *testing.T) {
	apps := []config.AppConfig{{
		App:         "myapp",
		Environment: "dev",
		ECRRepo:     "123456789012.dkr.ecr.us-east-1.amazonaws.com/myapp",
	}}
	h, rdb := newTestWebhookHandler(t, apps)
	ctx := context.Background()

	eb := EventBridgeEvent{
		Source:     "aws.ecr",
		DetailType: "ECR Image Action",
		Detail: ECRPushDetail{
			ActionType:     "DELETE",
			Result:         "SUCCESS",
			RepositoryName: "myapp",
			ImageTag:       "v1.0.0",
		},
	}
	body, _ := json.Marshal(eb)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ecr", bytes.NewReader(body))
	req.Header.Set("x-api-key", testAPIKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// No event should be enqueued for non-PUSH.
	msgs, err := rdb.XRange(ctx, queue.StreamKeyECR, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(msgs))
	}
}

func TestWebhook_NoMatchingApp(t *testing.T) {
	apps := []config.AppConfig{{
		App:         "other",
		Environment: "dev",
		ECRRepo:     "123456789012.dkr.ecr.us-east-1.amazonaws.com/other",
	}}
	h, rdb := newTestWebhookHandler(t, apps)
	ctx := context.Background()

	body := validECRPushBody("unknown-repo", "v1.0.0")
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ecr", bytes.NewReader(body))
	req.Header.Set("x-api-key", testAPIKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	msgs, err := rdb.XRange(ctx, queue.StreamKeyECR, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(msgs))
	}
}
