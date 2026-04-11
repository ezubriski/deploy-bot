package ecrpoller

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"

	"go.uber.org/zap"

	"github.com/ezubriski/deploy-bot/internal/metrics"
)

// WebhookHandler handles HTTP POST requests carrying EventBridge ECR push
// events. It authenticates via API key and delegates to Poller.ProcessEvent.
type WebhookHandler struct {
	poller  *Poller
	apiKey  []byte
	metrics *metrics.Metrics
	log     *zap.Logger
}

// NewWebhookHandler creates a handler that validates the x-api-key header
// against apiKey and processes events via the given Poller.
func NewWebhookHandler(poller *Poller, apiKey string, m *metrics.Metrics, log *zap.Logger) *WebhookHandler {
	return &WebhookHandler{
		poller:  poller,
		apiKey:  []byte(apiKey),
		metrics: m,
		log:     log,
	}
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.metrics.RecordWebhookRequest("ecr", "405")
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Constant-time API key comparison.
	provided := []byte(r.Header.Get("x-api-key"))
	if subtle.ConstantTimeCompare(provided, h.apiKey) != 1 {
		h.metrics.RecordWebhookRequest("ecr", "401")
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
	var eb EventBridgeEvent
	if err := json.NewDecoder(r.Body).Decode(&eb); err != nil {
		h.metrics.RecordWebhookRequest("ecr", "400")
		h.log.Warn("webhook: bad request body", zap.Error(err))
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	matched, enqueued := h.poller.ProcessEvent(r.Context(), eb)

	h.metrics.RecordWebhookRequest("ecr", "200")
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, `{"status":"ok","matched":%d,"enqueued":%d}`, matched, enqueued); err != nil {
		h.log.Warn("webhook: write response", zap.Error(err))
	}
}
