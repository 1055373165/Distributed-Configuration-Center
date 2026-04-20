package bench

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"time"
)

// Addr is one cluster endpoint, e.g. "127.0.0.1:8080".
type Addr string

// Scenario is a single workload pattern. Implementations must be safe for
// concurrent Step() calls from many goroutines — typically by using atomic
// counters or a RNG with its own mutex per caller.
//
// Lifecycle:
//
//	Setup    — prepare state (prepopulate keys, warm caches). Called once.
//	Step     — execute one unit of work. Called in a tight loop by workers.
//	Teardown — release resources. Called once after all workers stop.
type Scenario interface {
	Name() string
	Setup(ctx context.Context) error
	Step(ctx context.Context) (statusCode int, err error)
	Teardown(ctx context.Context) error
}

// ScenarioConfig groups the knobs every built-in scenario cares about.
//
// Addrs[0] is always treated as the "write endpoint" (writes to followers
// are transparently forwarded by RaftServer, but sending directly to the
// leader avoids an extra HTTP hop and reduces measurement noise). Reads
// fan out across all Addrs to balance load.
type ScenarioConfig struct {
	Addrs     []Addr
	Tenant    string
	Namespace string
	NumKeys   int           // key-pool size; higher = less hot-spotting
	ValueSize int           // PUT body size in bytes
	Timeout   time.Duration // per-request timeout (client-side)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// newHTTPClient builds a client tuned for load testing:
//   - Large idle-conn pool so high-concurrency workloads don't thrash the
//     3-way handshake.
//   - HTTP/1.1 (ForceAttemptHTTP2=false) to avoid HTTP/2 head-of-line
//     blocking on a single TCP connection — that's the right choice when
//     you're measuring tail latency.
//   - Compression off: responses are already small JSON and we don't want
//     CPU cost on the client skewing throughput.
func newHTTPClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        4096,
		MaxIdleConnsPerHost: 4096,
		MaxConnsPerHost:     4096,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
		ForceAttemptHTTP2:   false,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// makeKeyPool materializes n deterministic, well-distributed keys. Using a
// fixed pool (instead of allocating per Step) keeps allocations out of the
// hot path and makes runs reproducible.
func makeKeyPool(tenant, ns string, n int) []string {
	ks := make([]string, n)
	for i := 0; i < n; i++ {
		ks[i] = fmt.Sprintf("%s/%s/k%08d", tenant, ns, i)
	}
	return ks
}

// makeValue returns a reproducible byte blob of the given size.
// Deterministic seed -> identical blob across runs -> comparable numbers.
func makeValue(size int, seed int64) []byte {
	rnd := rand.New(rand.NewSource(seed))
	b := make([]byte, size)
	for i := range b {
		b[i] = byte('a' + rnd.Intn(26))
	}
	return b
}
