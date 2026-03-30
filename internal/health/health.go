package health

import (
	"net/http"
	"sync/atomic"
)

// Handler serves /healthz (liveness) and /readyz (readiness).
//
// Liveness is always 200 — if the process is running, it's alive.
//
// Readiness is 200 only when the pod is the active leader AND the ECR cache
// has completed its first populate. Follower pods remain not-ready
// intentionally; they are warm standbys with no active work to do.
type Handler struct {
	isLeader     atomic.Bool
	isCacheReady atomic.Bool
}

// SetLeader marks this pod as the active leader (true) or a follower (false).
func (h *Handler) SetLeader(v bool) {
	h.isLeader.Store(v)
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

// Readiness handles GET /readyz. Returns 200 if this pod is the leader and
// the ECR cache is populated; 503 otherwise.
func (h *Handler) Readiness(w http.ResponseWriter, r *http.Request) {
	if h.isLeader.Load() && h.isCacheReady.Load() {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte("not ready"))
}
