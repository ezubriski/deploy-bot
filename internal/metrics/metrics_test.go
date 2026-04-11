package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newTestMetrics(t *testing.T) *Metrics {
	t.Helper()
	reg := prometheus.NewRegistry()
	return New(reg)
}

func TestRecordDeploy(t *testing.T) {
	m := newTestMetrics(t)

	m.RecordDeploy("myapp", "requested")
	m.RecordDeploy("myapp", "requested")
	m.RecordDeploy("myapp", "approved")
	m.RecordDeploy("otherapp", "rejected")

	if got := testutil.ToFloat64(m.DeploysTotal.WithLabelValues("myapp", "requested")); got != 2 {
		t.Errorf("myapp/requested = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.DeploysTotal.WithLabelValues("myapp", "approved")); got != 1 {
		t.Errorf("myapp/approved = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.DeploysTotal.WithLabelValues("otherapp", "rejected")); got != 1 {
		t.Errorf("otherapp/rejected = %v, want 1", got)
	}
	// Label combination that was never touched should be 0.
	if got := testutil.ToFloat64(m.DeploysTotal.WithLabelValues("myapp", "expired")); got != 0 {
		t.Errorf("myapp/expired = %v, want 0", got)
	}
}

func TestRecordECRCache(t *testing.T) {
	m := newTestMetrics(t)

	m.RecordECRCacheHit("myapp")
	m.RecordECRCacheHit("myapp")
	m.RecordECRCacheMiss("myapp")
	m.RecordECRCacheMiss("otherapp")

	if got := testutil.ToFloat64(m.ECRCacheHits.WithLabelValues("myapp")); got != 2 {
		t.Errorf("cache hits myapp = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.ECRCacheMisses.WithLabelValues("myapp")); got != 1 {
		t.Errorf("cache misses myapp = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ECRCacheMisses.WithLabelValues("otherapp")); got != 1 {
		t.Errorf("cache misses otherapp = %v, want 1", got)
	}
}

func TestObserveECRRefresh(t *testing.T) {
	m := newTestMetrics(t)

	m.ObserveECRRefresh("myapp", 250*time.Millisecond)
	m.ObserveECRRefresh("myapp", 500*time.Millisecond)

	// CollectAndCount returns the number of metric series exposed by the
	// collector; a non-zero value confirms observations were recorded without
	// panicking.
	if n := testutil.CollectAndCount(m.ECRRefreshDuration); n == 0 {
		t.Error("expected ECRRefreshDuration to have series after observations")
	}
}

func TestSetPendingDeploys(t *testing.T) {
	m := newTestMetrics(t)

	m.SetPendingDeploys(3)
	if got := testutil.ToFloat64(m.PendingDeploys); got != 3 {
		t.Errorf("pending = %v, want 3", got)
	}

	m.SetPendingDeploys(1)
	if got := testutil.ToFloat64(m.PendingDeploys); got != 1 {
		t.Errorf("pending after decrement = %v, want 1", got)
	}

	m.SetPendingDeploys(0)
	if got := testutil.ToFloat64(m.PendingDeploys); got != 0 {
		t.Errorf("pending after zero = %v, want 0", got)
	}
}

func TestRecordArgoCDNotification(t *testing.T) {
	m := newTestMetrics(t)

	// Known triggers preserve their label value.
	m.RecordArgoCDNotification("on-sync-succeeded", ArgoCDResultMatched)
	m.RecordArgoCDNotification("on-sync-failed", ArgoCDResultMatched)
	m.RecordArgoCDNotification("on-health-degraded", ArgoCDResultUnmatched)
	m.RecordArgoCDNotification("on-sync-succeeded", ArgoCDResultNoHandleSkipped)

	// Unknown trigger collapses to "other" so label cardinality stays
	// bounded regardless of what the upstream controller ships.
	m.RecordArgoCDNotification("on-custom-weird-thing", ArgoCDResultUnhandledTrigger)
	m.RecordArgoCDNotification("", ArgoCDResultLookupError)

	cases := []struct {
		trigger, result string
		want            float64
	}{
		{"on-sync-succeeded", ArgoCDResultMatched, 1},
		{"on-sync-failed", ArgoCDResultMatched, 1},
		{"on-health-degraded", ArgoCDResultUnmatched, 1},
		{"on-sync-succeeded", ArgoCDResultNoHandleSkipped, 1},
		{"other", ArgoCDResultUnhandledTrigger, 1},
		{"other", ArgoCDResultLookupError, 1},
		// Label combinations that should NOT have been touched.
		{"on-sync-failed", ArgoCDResultUnmatched, 0},
		{"on-health-degraded", ArgoCDResultMatched, 0},
	}
	for _, tc := range cases {
		got := testutil.ToFloat64(m.ArgoCDNotificationsTotal.WithLabelValues(tc.trigger, tc.result))
		if got != tc.want {
			t.Errorf("{trigger=%q, result=%q} = %v, want %v", tc.trigger, tc.result, got, tc.want)
		}
	}
}

func TestRegistrationDoesNotPanic(t *testing.T) {
	// Registering twice on the same registry should panic; separate registries
	// should be fine.
	reg1 := prometheus.NewRegistry()
	reg2 := prometheus.NewRegistry()
	_ = New(reg1)
	_ = New(reg2) // must not panic
}
