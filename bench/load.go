package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LoadConfig describes one benchmark invocation.
//
//   - Concurrency: number of worker goroutines. In closed-loop mode each
//     worker issues the next request as soon as the previous one returns.
//   - Duration: measurement window after warm-up.
//   - WarmUp: samples observed during this window are discarded — important
//     for avoiding GC ramp-up, connection-pool warm, and bbolt mmap fault-ins
//     polluting the steady-state numbers.
//   - RPSCap: optional aggregate RPS ceiling (all workers share one ticker).
//     0 = unlimited (closed-loop saturation).
type LoadConfig struct {
	Concurrency int
	Duration    time.Duration
	WarmUp      time.Duration
	RPSCap      int
}

// Result is the serializable outcome of one Run.
//
// Durations are carried as time.Duration (JSON-encoded as nanosecond ints),
// which makes result files trivially diffable and machine-readable.
type Result struct {
	Scenario    string        `json:"scenario"`
	StartedAt   time.Time     `json:"started_at"`
	Duration    time.Duration `json:"duration_ns"`
	Concurrency int           `json:"concurrency"`
	RPSCap      int           `json:"rps_cap,omitempty"`
	Count       uint64        `json:"count"`
	Errors      uint64        `json:"errors"`
	StatusClass [6]uint64     `json:"status_class"`
	RPS         float64       `json:"rps"`
	Latency     Snapshot      `json:"latency"`
	Env         Env           `json:"env"`
}

// Run executes sc under the given load and returns the aggregated result.
//
// The heart of the implementation is the per-worker histogram pattern:
// each goroutine owns an unlocked Hist, writes to it on the hot path, and
// only after all workers finish do we merge them into one aggregate. This
// eliminates the single biggest source of measurement artefacts in
// high-concurrency benchmarks — the histogram lock itself.
//
// Warm-up is handled by each worker checking its request's timestamp
// against measureStart; this keeps the hot path branch-free of global
// resets and preserves the invariant that latency accounting is
// monotonic within a single worker.
func Run(ctx context.Context, sc Scenario, lc LoadConfig) (*Result, error) {
	if lc.Concurrency <= 0 {
		lc.Concurrency = 1
	}
	if lc.Duration <= 0 {
		lc.Duration = 10 * time.Second
	}

	if err := sc.Setup(ctx); err != nil {
		return nil, fmt.Errorf("scenario setup: %w", err)
	}
	defer sc.Teardown(ctx)

	hists := make([]*Hist, lc.Concurrency)
	for i := range hists {
		hists[i] = NewHist()
	}
	counters := &Counters{}

	// Shared rate limiter. One ticker means workers contend briefly on the
	// channel receive, but that's cheap compared to per-worker tickers —
	// and it's the only way to enforce an aggregate RPS ceiling precisely.
	var ticker *time.Ticker
	if lc.RPSCap > 0 {
		interval := time.Second / time.Duration(lc.RPSCap)
		if interval <= 0 {
			interval = time.Nanosecond
		}
		ticker = time.NewTicker(interval)
		defer ticker.Stop()
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	started := make(chan struct{})
	startedAt := time.Now()
	measureStart := startedAt.Add(lc.WarmUp)

	for i := 0; i < lc.Concurrency; i++ {
		wg.Add(1)
		go func(h *Hist) {
			defer wg.Done()
			<-started // gate: let every worker take off together
			for runCtx.Err() == nil {
				if ticker != nil {
					select {
					case <-ticker.C:
					case <-runCtx.Done():
						return
					}
				}
				t0 := time.Now()
				status, err := sc.Step(runCtx)
				lat := time.Since(t0)
				// Warm-up samples are discarded: they are dominated by GC
				// bootstrap, TCP handshakes, and bbolt page faults — not
				// representative of steady-state behaviour.
				if t0.Before(measureStart) {
					continue
				}
				// Shutdown-induced errors (request aborted because the run
				// ended mid-flight) are not the server's fault — drop them
				// so they don't show up as "errors" in the report.
				if err != nil && runCtx.Err() != nil {
					continue
				}
				if err != nil {
					counters.AddStatus(0)
				} else {
					counters.AddStatus(status)
				}
				h.Observe(lat)
			}
		}(hists[i])
	}
	close(started)

	// Run for warm-up + measurement, then shut down the workers and wait
	// for them to drain their in-flight request (up to scenario timeout).
	totalRun := lc.WarmUp + lc.Duration
	select {
	case <-time.After(totalRun):
	case <-ctx.Done():
	}
	cancel()
	wg.Wait()

	// Actual measurement window (may exceed lc.Duration slightly due to
	// in-flight requests draining after cancel).
	actualDur := time.Since(measureStart)
	if actualDur <= 0 {
		actualDur = lc.Duration
	}

	total := NewHist()
	for _, h := range hists {
		total.Merge(h)
	}

	res := &Result{
		Scenario:    sc.Name(),
		StartedAt:   measureStart,
		Duration:    actualDur,
		Concurrency: lc.Concurrency,
		RPSCap:      lc.RPSCap,
		Count:       total.Count(),
		Errors:      counters.Err(),
		StatusClass: counters.ByClass(),
		RPS:         float64(total.Count()) / actualDur.Seconds(),
		Latency:     total.Snapshot(),
		Env:         CollectEnv(),
	}
	return res, nil
}

// SaveJSON writes r to path in indented JSON. Directories are created as needed.
func SaveJSON(r *Result, path string) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// LoadJSON reads a Result previously written by SaveJSON.
func LoadJSON(path string) (*Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &r, nil
}
