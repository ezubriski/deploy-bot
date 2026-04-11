package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds all instrumentation for deploy-bot. Use New to create one
// with a custom registry (useful in tests) or NewDefault to register against
// prometheus.DefaultRegisterer.
type Metrics struct {
	DeploysTotal             *prometheus.CounterVec
	ECRCacheHits             *prometheus.CounterVec
	ECRCacheMisses           *prometheus.CounterVec
	ECRRefreshDuration       *prometheus.HistogramVec
	PendingDeploys           prometheus.Gauge
	WebhookRequestsTotal     *prometheus.CounterVec
	ArgoCDNotificationsTotal *prometheus.CounterVec
}

func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		DeploysTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "deploybot_deploys_total",
			Help: "Total deployment events by app and result.",
		}, []string{"app", "result"}),

		ECRCacheHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "deploybot_ecr_cache_hits_total",
			Help: "ECR tag cache hits by app.",
		}, []string{"app"}),

		ECRCacheMisses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "deploybot_ecr_cache_misses_total",
			Help: "ECR tag cache misses (fell back to direct ECR lookup) by app.",
		}, []string{"app"}),

		ECRRefreshDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "deploybot_ecr_refresh_duration_seconds",
			Help:    "Duration of ECR tag cache refresh by app.",
			Buckets: prometheus.DefBuckets,
		}, []string{"app"}),

		PendingDeploys: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "deploybot_pending_deployments",
			Help: "Current number of deployments awaiting approval.",
		}),

		WebhookRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "deploybot_webhook_requests_total",
			Help: "Total inbound webhook requests by webhook source (ecr, argocd) and HTTP status.",
		}, []string{"webhook", "status"}),

		ArgoCDNotificationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "deploybot_argocd_notifications_total",
			Help: "ArgoCD lifecycle notifications processed by the worker, labeled by trigger and outcome. Result values: matched (correlated to history, posted), unmatched (no history entry for sha), lookup_error (redis lookup failed), no_handle_skipped (sync-succeeded dropped because history entry has no slack handle), unhandled_trigger (trigger name not wired in bot), transient_rollout_skipped (health-degraded dropped for a fresh deploy whose payload had no actually-degraded sub-resources — assumed to be an argocd-reconciler roll-up artifact during a healthy rollout).",
		}, []string{"trigger", "result"}),
	}

	reg.MustRegister(
		m.DeploysTotal,
		m.ECRCacheHits,
		m.ECRCacheMisses,
		m.ECRRefreshDuration,
		m.PendingDeploys,
		m.WebhookRequestsTotal,
		m.ArgoCDNotificationsTotal,
	)

	return m
}

// NewDefault registers metrics against prometheus.DefaultRegisterer.
func NewDefault() *Metrics {
	return New(prometheus.DefaultRegisterer)
}

// RecordDeploy increments the deploy counter for the given app and result.
// result should be one of: requested, approved, rejected, expired, cancelled.
func (m *Metrics) RecordDeploy(app, result string) {
	m.DeploysTotal.WithLabelValues(app, result).Inc()
}

// RecordECRCacheHit increments the cache hit counter for the given app.
func (m *Metrics) RecordECRCacheHit(app string) {
	m.ECRCacheHits.WithLabelValues(app).Inc()
}

// RecordECRCacheMiss increments the cache miss counter for the given app.
func (m *Metrics) RecordECRCacheMiss(app string) {
	m.ECRCacheMisses.WithLabelValues(app).Inc()
}

// ObserveECRRefresh records the duration of a cache refresh for the given app.
func (m *Metrics) ObserveECRRefresh(app string, d time.Duration) {
	m.ECRRefreshDuration.WithLabelValues(app).Observe(d.Seconds())
}

// SetPendingDeploys sets the pending deployments gauge to n.
func (m *Metrics) SetPendingDeploys(n int) {
	m.PendingDeploys.Set(float64(n))
}

// RecordWebhookRequest increments the webhook request counter for the given
// source (e.g. "ecr", "argocd") and HTTP status code.
func (m *Metrics) RecordWebhookRequest(webhook, status string) {
	m.WebhookRequestsTotal.WithLabelValues(webhook, status).Inc()
}

// Known ArgoCD notification result values for RecordArgoCDNotification. Kept
// as constants so callers cannot typo a label value into a new series.
const (
	ArgoCDResultMatched          = "matched"
	ArgoCDResultUnmatched        = "unmatched"
	ArgoCDResultLookupError      = "lookup_error"
	ArgoCDResultNoHandleSkipped  = "no_handle_skipped"
	ArgoCDResultUnhandledTrigger = "unhandled_trigger"
	// ArgoCDResultTransientRolloutSkipped is recorded when an
	// on-health-degraded notification is dropped because it arrived for
	// a freshly-completed deploy with no actually-degraded sub-resources
	// in the payload — the fingerprint of an argocd-reconciler
	// roll-up artifact during a healthy Deployment rollout. See
	// internal/bot/argocd.go isTransientRolloutDegraded for the exact
	// gate and docs/argocd-notifications.md for operator tuning notes.
	ArgoCDResultTransientRolloutSkipped = "transient_rollout_skipped"
)

// RecordArgoCDNotification increments the ArgoCD notification outcome
// counter. trigger is normalized to one of the known trigger names
// (on-sync-succeeded, on-sync-failed, on-health-degraded) or "other" —
// an unbounded upstream trigger value would blow up label cardinality.
func (m *Metrics) RecordArgoCDNotification(trigger, result string) {
	m.ArgoCDNotificationsTotal.WithLabelValues(normalizeArgoCDTrigger(trigger), result).Inc()
}

// normalizeArgoCDTrigger coerces an arbitrary trigger string into one of
// the fixed set the bot knows about, collapsing everything else to
// "other". This keeps the trigger label cardinality bounded regardless
// of what the upstream argocd-notifications controller ships.
func normalizeArgoCDTrigger(trigger string) string {
	switch trigger {
	case "on-sync-succeeded", "on-sync-failed", "on-health-degraded":
		return trigger
	default:
		return "other"
	}
}
