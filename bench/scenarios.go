package bench

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// write_only
// ---------------------------------------------------------------------------

// WriteOnly hammers PUT /api/v1/config/<key> on the leader.
//
// What this measures:
//   - Raft log-replication throughput (Leader.Apply → AppendEntries → quorum
//     ack → FSM.Apply).
//   - BoltDB write-tx latency (single-writer + fsync per commit).
//   - WatchCache append contention.
//
// Why it's the most important number: it's the floor — mixed workloads can
// only ever get faster than this on the write portion.
type WriteOnly struct {
	cfg      ScenarioConfig
	keys     []string
	value    []byte
	client   *http.Client
	writeURL string
	counter  uint64
}

// NewWriteOnly constructs a WriteOnly scenario with sensible defaults.
func NewWriteOnly(cfg ScenarioConfig) *WriteOnly {
	if cfg.NumKeys == 0 {
		cfg.NumKeys = 10_000
	}
	if cfg.ValueSize == 0 {
		cfg.ValueSize = 64
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &WriteOnly{
		cfg:      cfg,
		keys:     makeKeyPool(cfg.Tenant, cfg.Namespace, cfg.NumKeys),
		value:    makeValue(cfg.ValueSize, 1),
		client:   newHTTPClient(cfg.Timeout),
		writeURL: fmt.Sprintf("http://%s/api/v1/config/", cfg.Addrs[0]),
	}
}

// Name implements Scenario.
func (s *WriteOnly) Name() string { return "write_only" }

// Setup is a no-op: keys are created lazily on first PUT.
func (s *WriteOnly) Setup(ctx context.Context) error { return nil }

// Step issues one PUT. Round-robins over the key pool so we exercise real
// replication instead of hot-spotting a single key (which would bypass the
// cost of new entry creation).
func (s *WriteOnly) Step(ctx context.Context) (int, error) {
	i := atomic.AddUint64(&s.counter, 1)
	key := s.keys[int(i)%len(s.keys)]
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.writeURL+key, bytes.NewReader(s.value))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Teardown is a no-op; keys are left in place for inspection.
func (s *WriteOnly) Teardown(ctx context.Context) error { return nil }

// ---------------------------------------------------------------------------
// read_only
// ---------------------------------------------------------------------------

// ReadOnly fans GET /api/v1/config/<key> across every endpoint in Addrs.
//
// What this measures:
//   - Stale-read path: HTTP -> BoltStore.Get -> JSON encode.
//   - No Raft participation — pure read-side scalability.
//
// Sample count across N nodes should scale ~linearly until the client or
// loopback is saturated. If it doesn't, the server-side read path is the
// bottleneck (most likely JSON encoding).
type ReadOnly struct {
	cfg      ScenarioConfig
	keys     []string
	client   *http.Client
	readURLs []string
	counter  uint64
	rrIdx    uint64
}

// NewReadOnly constructs a ReadOnly scenario.
func NewReadOnly(cfg ScenarioConfig) *ReadOnly {
	if cfg.NumKeys == 0 {
		cfg.NumKeys = 1_000
	}
	if cfg.ValueSize == 0 {
		cfg.ValueSize = 64
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	urls := make([]string, len(cfg.Addrs))
	for i, a := range cfg.Addrs {
		urls[i] = fmt.Sprintf("http://%s/api/v1/config/", a)
	}
	return &ReadOnly{
		cfg:      cfg,
		keys:     makeKeyPool(cfg.Tenant, cfg.Namespace, cfg.NumKeys),
		client:   newHTTPClient(cfg.Timeout),
		readURLs: urls,
	}
}

// Name implements Scenario.
func (s *ReadOnly) Name() string { return "read_only" }

// Setup prepopulates the key pool on the leader and waits briefly for
// followers to catch up so stale reads return real data.
func (s *ReadOnly) Setup(ctx context.Context) error {
	writeURL := fmt.Sprintf("http://%s/api/v1/config/", s.cfg.Addrs[0])
	value := makeValue(s.cfg.ValueSize, 2)
	for _, k := range s.keys {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, writeURL+k, bytes.NewReader(value))
		if err != nil {
			return err
		}
		resp, err := s.client.Do(req)
		if err != nil {
			return fmt.Errorf("prepopulate %s: %w", k, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("prepopulate %s: status %d", k, resp.StatusCode)
		}
	}
	// Give followers a moment to apply the replicated entries before we
	// start issuing stale reads against them.
	time.Sleep(500 * time.Millisecond)
	return nil
}

// Step issues one GET, round-robining across all Addrs.
func (s *ReadOnly) Step(ctx context.Context) (int, error) {
	i := atomic.AddUint64(&s.counter, 1)
	key := s.keys[int(i)%len(s.keys)]
	u := s.readURLs[int(atomic.AddUint64(&s.rrIdx, 1))%len(s.readURLs)]
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+key, nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Teardown is a no-op.
func (s *ReadOnly) Teardown(ctx context.Context) error { return nil }

// ---------------------------------------------------------------------------
// mixed
// ---------------------------------------------------------------------------

// Mixed interleaves GETs and PUTs in a configurable ratio — the closest
// approximation to real configuration-center traffic (typically 95%+ reads).
//
// Implementation note: we compose the two primitive scenarios rather than
// reinventing the wheel; this keeps behaviour (key pool, HTTP client tuning,
// forwarded writes) identical across scenarios.
type Mixed struct {
	cfg         ScenarioConfig
	readPercent int
	r           *ReadOnly
	w           *WriteOnly
	counter     uint64
}

// NewMixed constructs a Mixed scenario with the given read percentage (0–100).
func NewMixed(cfg ScenarioConfig, readPercent int) *Mixed {
	return &Mixed{
		cfg:         cfg,
		readPercent: readPercent,
		r:           NewReadOnly(cfg),
		w:           NewWriteOnly(cfg),
	}
}

// Name returns "mixed_<R>r_<W>w", e.g. "mixed_95r_5w".
func (s *Mixed) Name() string {
	return fmt.Sprintf("mixed_%dr_%dw", s.readPercent, 100-s.readPercent)
}

// Setup delegates to the read scenario, which prepopulates keys.
func (s *Mixed) Setup(ctx context.Context) error { return s.r.Setup(ctx) }

// Step dispatches to the read or write sub-scenario based on a simple modulo
// of the request counter. For R=95 this gives an exact 95/5 split; no RNG
// jitter means the ratio is stable across short runs.
func (s *Mixed) Step(ctx context.Context) (int, error) {
	i := atomic.AddUint64(&s.counter, 1)
	if int(i%100) < s.readPercent {
		return s.r.Step(ctx)
	}
	return s.w.Step(ctx)
}

// Teardown is a no-op.
func (s *Mixed) Teardown(ctx context.Context) error { return nil }
