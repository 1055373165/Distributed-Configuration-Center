package bench

import "sync/atomic"

// Counters tracks request outcomes across all workers. Indexed by
// status-class bucket (code/100) so we can tell apart transport errors (0),
// 2xx successes, and 5xx overloads at a glance.
//
// Transport-level errors (nil response, connection refused) count as class 0
// and as an error. HTTP 4xx/5xx count in their respective class and as errors;
// 2xx/3xx count as OK.
type Counters struct {
	ok     uint64
	err    uint64
	status [6]uint64 // 0=transport err, 1=1xx, 2=2xx, 3=3xx, 4=4xx, 5=5xx
}

// AddStatus records a single response. code=0 means "no response received".
func (c *Counters) AddStatus(code int) {
	idx := 0
	if code > 0 {
		idx = code / 100
		if idx < 0 || idx >= len(c.status) {
			idx = 0
		}
	}
	atomic.AddUint64(&c.status[idx], 1)
	if code >= 200 && code < 400 {
		atomic.AddUint64(&c.ok, 1)
	} else {
		atomic.AddUint64(&c.err, 1)
	}
}

// Ok returns the successful-response count.
func (c *Counters) Ok() uint64 { return atomic.LoadUint64(&c.ok) }

// Err returns the failure count (transport errors + HTTP 4xx/5xx).
func (c *Counters) Err() uint64 { return atomic.LoadUint64(&c.err) }

// ByClass returns a snapshot of per-status-class counts.
func (c *Counters) ByClass() [6]uint64 {
	var r [6]uint64
	for i := range r {
		r[i] = atomic.LoadUint64(&c.status[i])
	}
	return r
}
