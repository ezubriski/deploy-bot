package health

import (
	"net/http"
	"sync/atomic"
)

// Handler serves /healthz (liveness) and /readyz (readiness).
//
// Liveness is always 200 — if the process is running, it's alive.
//
// Readiness is 200 only after the ECR cache has completed its first populate.
type Handler struct {
	isCacheReady atomic.Bool
}

// SetCacheReady marks the ECR cache as having completed its first populate.
// This is a one-way latch — once set it is never cleared.
func (h *Handler) SetCacheReady() {
	h.isCacheReady.Store(true)
}

// Liveness handles GET /healthz. Always returns 200 OK.
func (h *Handler) Liveness(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// Readiness handles GET /readyz. Returns 200 once the ECR cache is populated.
func (h *Handler) Readiness(w http.ResponseWriter, r *http.Request) {
	if h.isCacheReady.Load() {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte("not ready"))
}
