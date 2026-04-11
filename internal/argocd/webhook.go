package argocd

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/metrics"
)

// secretHeader is the HTTP header argocd-notifications uses to deliver our
// shared secret. Matches the reference template in
// deploy/argocd-notifications/templates.yaml.
const secretHeader = "X-Deploybot-Secret"

// maxBodySize bounds the request body the handler will read. ArgoCD
// notification payloads are typically a few KiB; 1 MiB is generous and
// matches the ECR webhook precedent.
const maxBodySize = 1 << 20 // 1 MiB

// WebhookHandler accepts HTTP POST requests carrying ArgoCD lifecycle
// notifications, validates a shared secret, and delegates to a Processor
// which dedupes and enqueues. The handler does no Slack posting and no
// GitHub lookups — it returns 2xx fast, matching the argocd-notifications
// retry contract (no DLQ, retries only on 5xx and network errors).
type WebhookHandler struct {
	processor *Processor
	secret    []byte
	metrics   *metrics.Metrics
	log       *zap.Logger
}

// NewWebhookHandler constructs a handler that validates secret against the
// X-Deploybot-Secret header. Caller is responsible for ensuring secret has
// already passed the >= 32 char length check.
func NewWebhookHandler(processor *Processor, secret string, m *metrics.Metrics, log *zap.Logger) *WebhookHandler {
	if log == nil {
		log = zap.NewNop()
	}
	return &WebhookHandler{
		processor: processor,
		secret:    []byte(secret),
		metrics:   m,
		log:       log,
	}
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.recordStatus("405")
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Constant-time secret comparison. Done before reading the body so a
	// caller without the secret cannot exhaust our 1 MiB read budget.
	provided := []byte(r.Header.Get(secretHeader))
	if subtle.ConstantTimeCompare(provided, h.secret) != 1 {
		h.recordStatus("401")
		// Deliberately generic error: do not leak whether the header was
		// missing vs. wrong.
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	var payload WebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		// MaxBytesReader returns *http.MaxBytesError on overflow; surface
		// that as 413 so operators can distinguish a misconfigured
		// template from a malformed body.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.recordStatus("413")
			h.log.Warn("argocd webhook: body exceeded size limit", zap.Int64("limit", maxErr.Limit))
			http.Error(w, `{"error":"request body too large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		h.recordStatus("400")
		h.log.Warn("argocd webhook: bad request body", zap.Error(err))
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Lightweight payload sanity check. We do not require every field —
	// some triggers (e.g. on-health-degraded for an in-flight sync) may
	// not have a syncResult yet — but we do need a trigger and an
	// argocdApp to log against.
	if payload.Trigger == "" || payload.ArgoCDApp == "" {
		h.recordStatus("400")
		h.log.Warn("argocd webhook: missing required fields",
			zap.String("trigger", payload.Trigger),
			zap.String("argocd_app", payload.ArgoCDApp),
		)
		http.Error(w, `{"error":"trigger and argocdApp are required"}`, http.StatusBadRequest)
		return
	}

	res, err := h.processor.Process(r.Context(), payload)
	if err != nil {
		// Surface infrastructure failures (Redis dedupe down) as 5xx so
		// the controller retries. Stream-write failures are absorbed by
		// the buffer inside Process and do not reach here.
		h.recordStatus("500")
		h.log.Error("argocd webhook: process",
			zap.String("trigger", payload.Trigger),
			zap.String("argocd_app", payload.ArgoCDApp),
			zap.Error(err),
		)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	h.recordStatus("200")
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w,
		`{"status":"ok","recognized":%t,"deduped":%t,"enqueued":%t}`,
		res.Recognized, res.Deduped, res.Enqueued,
	); err != nil {
		h.log.Warn("argocd webhook: write response", zap.Error(err))
	}
}

// recordStatus is a thin wrapper around metrics.RecordWebhookRequest that
// no-ops when metrics are nil (lets tests construct a handler without a
// full metrics registry). Always records against the "argocd" webhook
// source, so operators can distinguish ArgoCD 401s from ECR 401s in the
// shared deploybot_webhook_requests_total counter.
func (h *WebhookHandler) recordStatus(status string) {
	if h.metrics == nil {
		return
	}
	h.metrics.RecordWebhookRequest("argocd", status)
}
