// Package metrics is the single source of truth for Prometheus metrics
// exposed by paladin-core.
//
// Design decisions:
//
//   - All metrics live in a single file so ops can grep for "paladin_"
//     and find the full catalogue. This beats scattering Register calls
//     across packages; we've seen that lead to duplicate-name panics and
//     orphaned series after refactors.
//
//   - Metric names follow Prometheus conventions: <namespace>_<subsystem>_
//     <name>_<unit>. Units are SI base units where possible (seconds, bytes).
//
//   - Latency uses a Histogram with buckets tuned for a config-center SLO
//     (p99 ≤ 50 ms reads, ≤ 200 ms writes). If you change these buckets,
//     bump the metric version and document it in docs/production-refactoring.md
//     because Prometheus requires a full scrape cycle to forget old buckets.
//
//   - We use a custom registry (not the default) so tests can create
//     isolated registries without global state leaking between runs.
//     A package-level Registry is exposed for the main binary to wire
//     into its /metrics handler.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// namespace and subsystems — keep flat; resist the urge to add a third level.
const (
	namespace        = "paladin"
	subsystemHTTP    = "http"
	subsystemRaft    = "raft"
	subsystemStore   = "store"
)

// Registry is the project-wide registry. Export /metrics from this
// registry; never use prometheus.DefaultRegisterer in production code.
var Registry = prometheus.NewRegistry()

// HTTPRequestsTotal counts HTTP requests by method/path/status. "path" is the
// route pattern (never the raw path) to avoid unbounded cardinality.
var HTTPRequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemHTTP,
		Name:      "requests_total",
		Help:      "HTTP requests processed, labeled by method, route, and status class.",
	},
	[]string{"method", "route", "status"},
)

// HTTPRequestDuration observes request latency. Buckets span 1ms → 10s.
var HTTPRequestDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: subsystemHTTP,
		Name:      "request_duration_seconds",
		Help:      "HTTP request latency in seconds, labeled by method and route.",
		// Buckets chosen for 1ms..10s span — covers fast reads through
		// slow forwards. 14 buckets keeps series count modest.
		Buckets: []float64{
			0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
		},
	},
	[]string{"method", "route"},
)

// RaftApplyTotal counts Raft Apply calls by op type and outcome.
var RaftApplyTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: subsystemRaft,
		Name:      "apply_total",
		Help:      "Raft Apply invocations, labeled by op type (put|delete) and outcome (ok|error|not_leader).",
	},
	[]string{"op", "outcome"},
)

// RaftApplyDuration observes Apply end-to-end latency (submit → commit → FSM).
// A spike here is the earliest signal of quorum stress or disk contention.
var RaftApplyDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: subsystemRaft,
		Name:      "apply_duration_seconds",
		Help:      "Raft Apply latency in seconds, labeled by op type.",
		Buckets: []float64{
			0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
		},
	},
	[]string{"op"},
)

// StoreRevision is the monotonically-increasing store revision.
// As a Gauge it's cheap to scrape and lets ops trend write throughput
// (rate(paladin_store_revision[1m])) without a separate counter.
var StoreRevision = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: subsystemStore,
		Name:      "revision",
		Help:      "Current store revision (monotonic; a Gauge for rate() ergonomics).",
	},
)

// init wires metrics into the package Registry exactly once. A sync.Once is
// unnecessary because package init runs once by spec; panicking on double
// register would indicate a programming error worth surfacing loudly.
func init() {
	Registry.MustRegister(
		HTTPRequestsTotal,
		HTTPRequestDuration,
		RaftApplyTotal,
		RaftApplyDuration,
		StoreRevision,
		// Go runtime + process collectors give heap, goroutines, fds "for free".
		// Skip these and you lose the cheapest incident-triage signals we have.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// StatusClass converts an HTTP status code to a coarse label ("2xx", "4xx", ...).
// Keeping the label coarse bounds the cardinality of HTTPRequestsTotal.
func StatusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}
