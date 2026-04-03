package health

import (
	"net/http"
	"sync/atomic"
)

// Handler serves /healthz (liveness) and /readyz (readiness).
//
// Liveness returns 200 only after SetHealthy is called. Before that (e.g.
// while waiting for Redis), it returns 503. This lets Kubernetes restart the
// pod if a required dependency never becomes available.
//
// Readiness returns 200 only after the ECR cache has completed its first
// populate.
type Handler struct {
	isHealthy    atomic.Bool
	isCacheReady atomic.Bool
}

// SetHealthy marks the pod as alive. Call this once core dependencies (Redis)
// are confirmed available.
func (h *Handler) SetHealthy() {
	h.isHealthy.Store(true)
}

// SetCacheReady marks the ECR cache as having completed its first populate.
// This is a one-way latch — once set it is never cleared.
func (h *Handler) SetCacheReady() {
	h.isCacheReady.Store(true)
}

// Liveness handles GET /healthz. Returns 200 once SetHealthy has been called.
func (h *Handler) Liveness(w http.ResponseWriter, r *http.Request) {
	if h.isHealthy.Load() {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte("not healthy"))
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
