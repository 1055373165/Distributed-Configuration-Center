package sdk

import "github.com/prometheus/client_golang/prometheus"

// clientMetrics holds the Prometheus metrics for a single SDK Client.
//
// Design: every Client owns its own prometheus.Registry — no hidden package
// globals, no registrerer conflicts when an app runs multiple Clients (e.g.
// one per tenant). Library consumers expose them via:
//
//	http.Handle("/sdk-metrics",
//	    promhttp.HandlerFor(c.MetricsRegistry(), promhttp.HandlerOpts{}))
//
// Or merge into a shared registry with prometheus.Gatherers{reg1, reg2, ...}.
//
// Tenant and namespace are ConstLabels, not per-observation labels. Keeping
// them constant (a) avoids cardinality explosion on the Client's side and
// (b) lets us skip WithLabelValues hot-path work.
type clientMetrics struct {
	registry *prometheus.Registry

	fullPullsTotal    *prometheus.CounterVec // outcome=ok|error
	fullPullDuration  prometheus.Histogram
	watchPollsTotal   *prometheus.CounterVec // outcome=events|empty|error
	watchPollDuration prometheus.Histogram
	watchEventsTotal  *prometheus.CounterVec // type=PUT|DELETE
	cacheLoadsTotal   *prometheus.CounterVec // outcome=ok|error|checksum_mismatch
	revision          prometheus.Gauge
	configs           prometheus.Gauge
}

// newClientMetrics constructs and registers SDK metrics into a fresh
// registry scoped to one tenant+namespace pair.
func newClientMetrics(tenant, namespace string) *clientMetrics {
	reg := prometheus.NewRegistry()
	labels := prometheus.Labels{"tenant": tenant, "namespace": namespace}

	m := &clientMetrics{
		registry: reg,
		fullPullsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "paladin_sdk",
			Name:        "full_pulls_total",
			Help:        "Total SDK full-pull attempts, labeled by outcome.",
			ConstLabels: labels,
		}, []string{"outcome"}),
		fullPullDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace:   "paladin_sdk",
			Name:        "full_pull_duration_seconds",
			Help:        "Latency of SDK full-pull requests (success or error).",
			Buckets:     prometheus.DefBuckets,
			ConstLabels: labels,
		}),
		watchPollsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "paladin_sdk",
			Name:        "watch_polls_total",
			Help:        "Total SDK watch long-poll attempts, labeled by outcome.",
			ConstLabels: labels,
		}, []string{"outcome"}),
		watchPollDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "paladin_sdk",
			Name:      "watch_poll_duration_seconds",
			Help:      "Wall-clock duration of SDK watch long-poll requests. Timeouts show up at the PollTimeout bucket edge.",
			// Tuned for long-poll: p50 near PollTimeout for idle cases,
			// sub-second tails when events arrive quickly.
			Buckets:     []float64{0.01, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60},
			ConstLabels: labels,
		}),
		watchEventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "paladin_sdk",
			Name:        "watch_events_total",
			Help:        "Total config-change events applied via watch, labeled by type.",
			ConstLabels: labels,
		}, []string{"type"}),
		cacheLoadsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace:   "paladin_sdk",
			Name:        "cache_loads_total",
			Help:        "Total disk-cache load attempts, labeled by outcome.",
			ConstLabels: labels,
		}, []string{"outcome"}),
		revision: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   "paladin_sdk",
			Name:        "revision",
			Help:        "Current known revision of the SDK's local view. Lag vs server = replication/watch delay.",
			ConstLabels: labels,
		}),
		configs: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   "paladin_sdk",
			Name:        "configs",
			Help:        "Number of configs currently held in the SDK's in-memory store.",
			ConstLabels: labels,
		}),
	}

	reg.MustRegister(
		m.fullPullsTotal,
		m.fullPullDuration,
		m.watchPollsTotal,
		m.watchPollDuration,
		m.watchEventsTotal,
		m.cacheLoadsTotal,
		m.revision,
		m.configs,
	)
	return m
}
