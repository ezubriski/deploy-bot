package health

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLiveness_NotHealthyByDefault(t *testing.T) {
	h := &Handler{}
	assertStatus(t, h.Liveness, http.StatusServiceUnavailable)
}

func TestLiveness_HealthyAfterSet(t *testing.T) {
	h := &Handler{}
	h.SetHealthy()
	assertStatus(t, h.Liveness, http.StatusOK)
}

func TestReadiness_NotReadyUntilCachePopulated(t *testing.T) {
	h := &Handler{}
	assertStatus(t, h.Readiness, http.StatusServiceUnavailable)
}

func TestReadiness_ReadyAfterCachePopulated(t *testing.T) {
	h := &Handler{}
	h.SetCacheReady()
	assertStatus(t, h.Readiness, http.StatusOK)
}

func TestReadiness_CacheReadyIsOnceOnly(t *testing.T) {
	h := &Handler{}
	h.SetCacheReady()
	h.SetCacheReady()
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
