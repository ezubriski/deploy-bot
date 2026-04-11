package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds all instrumentation for deploy-bot. Use New to create one
// with a custom registry (useful in tests) or NewDefault to register against
// prometheus.DefaultRegisterer.
type Metrics struct {
	DeploysTotal         *prometheus.CounterVec
	ECRCacheHits         *prometheus.CounterVec
	ECRCacheMisses       *prometheus.CounterVec
	ECRRefreshDuration   *prometheus.HistogramVec
	PendingDeploys       prometheus.Gauge
	WebhookRequestsTotal *prometheus.CounterVec
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
	}

	reg.MustRegister(
		m.DeploysTotal,
		m.ECRCacheHits,
		m.ECRCacheMisses,
		m.ECRRefreshDuration,
		m.PendingDeploys,
		m.WebhookRequestsTotal,
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
