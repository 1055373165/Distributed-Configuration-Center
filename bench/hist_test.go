package bench

import (
	"math"
	"testing"
	"time"
)

func TestHistBasicDistribution(t *testing.T) {
	h := NewHist()
	// Uniform distribution 0..999 ms.
	for i := 0; i < 1000; i++ {
		h.Observe(time.Duration(i) * time.Millisecond)
	}
	if h.Count() != 1000 {
		t.Fatalf("count = %d, want 1000", h.Count())
	}

	p50 := h.Percentile(0.50)
	want50 := 500 * time.Millisecond
	if math.Abs(float64(p50-want50)) > float64(2*time.Millisecond) {
		t.Fatalf("p50 = %v, want ≈ %v", p50, want50)
	}

	p99 := h.Percentile(0.99)
	if p99 < 985*time.Millisecond || p99 > 995*time.Millisecond {
		t.Fatalf("p99 = %v, want ≈ 990ms", p99)
	}

	if h.Min() != 0 {
		t.Fatalf("min = %v, want 0", h.Min())
	}
	if h.Max() != 999*time.Millisecond {
		t.Fatalf("max = %v, want 999ms", h.Max())
	}
}

func TestHistMergePreservesStats(t *testing.T) {
	a, b := NewHist(), NewHist()
	for i := 0; i < 500; i++ {
		a.Observe(1 * time.Millisecond)
		b.Observe(5 * time.Millisecond)
	}
	m := NewHist()
	m.Merge(a)
	m.Merge(b)

	if m.Count() != 1000 {
		t.Fatalf("count = %d, want 1000", m.Count())
	}
	// Expected mean: (500×1ms + 500×5ms)/1000 = 3ms.
	mean := m.Mean()
	if mean < 2900*time.Microsecond || mean > 3100*time.Microsecond {
		t.Fatalf("mean = %v, want ≈ 3ms", mean)
	}
	if m.Min() != 1*time.Millisecond {
		t.Fatalf("min = %v, want 1ms", m.Min())
	}
	if m.Max() != 5*time.Millisecond {
		t.Fatalf("max = %v, want 5ms", m.Max())
	}
}

func TestHistOverflowRetainsMax(t *testing.T) {
	h := NewHist()
	h.Observe(30 * time.Second) // > 10s overflow range
	if h.Max() != 30*time.Second {
		t.Fatalf("max = %v, want 30s", h.Max())
	}
	// With only one overflow sample, every percentile returns the exact max.
	if got := h.Percentile(0.5); got != 30*time.Second {
		t.Fatalf("p50 on overflow = %v, want 30s", got)
	}
}

func TestHistEmptyPercentile(t *testing.T) {
	h := NewHist()
	if got := h.Percentile(0.99); got != 0 {
		t.Fatalf("percentile of empty = %v, want 0", got)
	}
	snap := h.Snapshot()
	if snap.Count != 0 || snap.Max != 0 || snap.Min != 0 {
		t.Fatalf("empty snapshot = %+v", snap)
	}
}
