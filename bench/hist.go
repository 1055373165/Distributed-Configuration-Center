// Package bench is PaladinCore's load-testing toolkit.
//
// Design principles (see bench/README.md for the full story):
//
//  1. Per-worker histograms merged at the end — no lock contention on the hot path.
//  2. Observe() records wall-clock latency; warm-up samples are filtered by the
//     caller using a timestamp, not by resetting state.
//  3. Linear buckets (100µs × 100k = 0–10s range) are enough for HTTP-scale
//     latencies. Values above 10s fall into the overflow bucket but min/max
//     are still tracked exactly so tail outliers are never silently dropped.
//
// If you need sub-µs resolution at high range, swap this for HdrHistogram.
package bench

import (
	"math"
	"time"
)

const (
	defaultBucketWidth = 100 * time.Microsecond
	defaultMaxLatency  = 10 * time.Second
)

// Hist is a fixed-resolution linear-bucket histogram.
//
// NOT safe for concurrent Observe(): each worker must own its instance.
// Merge() is used to aggregate across workers after the run.
type Hist struct {
	bucketWidth time.Duration
	buckets     []uint64 // last bucket captures overflow
	count       uint64
	sumNs       uint64
	minNs       uint64
	maxNs       uint64
}

// NewHist returns a histogram covering [0, 10s] at 100µs resolution.
func NewHist() *Hist {
	n := int(defaultMaxLatency/defaultBucketWidth) + 1
	return &Hist{
		bucketWidth: defaultBucketWidth,
		buckets:     make([]uint64, n),
		minNs:       math.MaxUint64,
	}
}

// Observe records one latency sample.
func (h *Hist) Observe(d time.Duration) {
	if d < 0 {
		d = 0
	}
	ns := uint64(d.Nanoseconds())
	i := int(d / h.bucketWidth)
	if i >= len(h.buckets) {
		i = len(h.buckets) - 1
	}
	h.buckets[i]++
	h.count++
	h.sumNs += ns
	if ns < h.minNs {
		h.minNs = ns
	}
	if ns > h.maxNs {
		h.maxNs = ns
	}
}

// Merge folds o's counts into h. Safe to call after both have stopped observing.
func (h *Hist) Merge(o *Hist) {
	if o == nil || o.count == 0 {
		return
	}
	for i, c := range o.buckets {
		h.buckets[i] += c
	}
	h.count += o.count
	h.sumNs += o.sumNs
	if o.minNs < h.minNs {
		h.minNs = o.minNs
	}
	if o.maxNs > h.maxNs {
		h.maxNs = o.maxNs
	}
}

// Percentile returns the duration at percentile p (0.0 ≤ p ≤ 1.0).
// Approximated by picking the middle of the bucket that contains the target
// rank; the overflow bucket returns the exact observed max.
func (h *Hist) Percentile(p float64) time.Duration {
	if h.count == 0 {
		return 0
	}
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	target := uint64(math.Ceil(float64(h.count) * p))
	if target == 0 {
		target = 1
	}
	var cum uint64
	for i, c := range h.buckets {
		cum += c
		if cum >= target {
			if i == len(h.buckets)-1 {
				return time.Duration(h.maxNs)
			}
			low := time.Duration(i) * h.bucketWidth
			high := low + h.bucketWidth
			return (low + high) / 2
		}
	}
	return time.Duration(h.maxNs)
}

// Count returns the number of samples observed.
func (h *Hist) Count() uint64 { return h.count }

// Mean returns the arithmetic mean latency.
func (h *Hist) Mean() time.Duration {
	if h.count == 0 {
		return 0
	}
	return time.Duration(h.sumNs / h.count)
}

// Min returns the smallest observed latency.
func (h *Hist) Min() time.Duration {
	if h.count == 0 {
		return 0
	}
	return time.Duration(h.minNs)
}

// Max returns the largest observed latency.
func (h *Hist) Max() time.Duration { return time.Duration(h.maxNs) }

// Snapshot is a JSON-friendly summary. Durations are serialized as
// nanoseconds for trivial diffing across runs.
type Snapshot struct {
	Count uint64        `json:"count"`
	Min   time.Duration `json:"min_ns"`
	Mean  time.Duration `json:"mean_ns"`
	P50   time.Duration `json:"p50_ns"`
	P90   time.Duration `json:"p90_ns"`
	P95   time.Duration `json:"p95_ns"`
	P99   time.Duration `json:"p99_ns"`
	P999  time.Duration `json:"p999_ns"`
	Max   time.Duration `json:"max_ns"`
}

// Snapshot computes every summary statistic in one pass.
func (h *Hist) Snapshot() Snapshot {
	return Snapshot{
		Count: h.count,
		Min:   h.Min(),
		Mean:  h.Mean(),
		P50:   h.Percentile(0.50),
		P90:   h.Percentile(0.90),
		P95:   h.Percentile(0.95),
		P99:   h.Percentile(0.99),
		P999:  h.Percentile(0.999),
		Max:   h.Max(),
	}
}
