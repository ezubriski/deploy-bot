package health

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLiveness_AlwaysOK(t *testing.T) {
	h := &Handler{}

	for _, desc := range []string{"follower", "leader", "leader+cache"} {
		t.Run(desc, func(t *testing.T) {
			if desc == "leader" || desc == "leader+cache" {
				h.SetLeader(true)
			}
			if desc == "leader+cache" {
				h.SetCacheReady()
			}
			assertStatus(t, h.Liveness, http.StatusOK)
		})
	}
}

func TestReadiness_FollowerNotReady(t *testing.T) {
	h := &Handler{} // leader=false, cache=false
	assertStatus(t, h.Readiness, http.StatusServiceUnavailable)
}

func TestReadiness_LeaderButNoCacheNotReady(t *testing.T) {
	h := &Handler{}
	h.SetLeader(true)
	assertStatus(t, h.Readiness, http.StatusServiceUnavailable)
}

func TestReadiness_CacheReadyButNotLeaderNotReady(t *testing.T) {
	h := &Handler{}
	h.SetCacheReady()
	assertStatus(t, h.Readiness, http.StatusServiceUnavailable)
}

func TestReadiness_LeaderAndCacheReady(t *testing.T) {
	h := &Handler{}
	h.SetLeader(true)
	h.SetCacheReady()
	assertStatus(t, h.Readiness, http.StatusOK)
}

func TestReadiness_LosesLeadership(t *testing.T) {
	h := &Handler{}
	h.SetLeader(true)
	h.SetCacheReady()
	assertStatus(t, h.Readiness, http.StatusOK)

	h.SetLeader(false)
	assertStatus(t, h.Readiness, http.StatusServiceUnavailable)
}

func TestReadiness_RegainsLeadership(t *testing.T) {
	h := &Handler{}
	h.SetLeader(true)
	h.SetCacheReady()
	h.SetLeader(false)
	h.SetLeader(true)
	// Cache was already set — should be ready again immediately.
	assertStatus(t, h.Readiness, http.StatusOK)
}

func assertStatus(t *testing.T, handler http.HandlerFunc, want int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler(rec, req)
	if got := rec.Code; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
}
