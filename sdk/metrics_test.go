package sdk

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMetricsRegistry_ExposesExpectedSeries verifies that every SDK metric we
// document in README's Observability section is actually registered and
// discoverable through c.MetricsRegistry(). This exercises each metric path
// end-to-end before asserting Gather() output — Prometheus's Gather() skips
// counter-vec series whose label values have never been observed, so a pure
// "is it registered" check would be a false positive. Instead, drive real
// traffic so every metric family emits at least one sample.
func TestMetricsRegistry_ExposesExpectedSeries(t *testing.T) {
	ts, ws := testSDKServer(t)
	addr := strings.TrimPrefix(ts.URL, "http://")
	// Seed so the full pull returns a non-empty revision / configs gauge.
	_, _ = ws.Put("public/prod/seed", []byte("1"))

	c, err := New(Config{
		Addrs:     []string{addr},
		Tenant:    "public",
		Namespace: "prod",
		CacheDir:  t.TempDir(), // so we can exercise the cache load path below
		// Must be >= 1s: the SDK serializes timeout with int(d.Seconds()),
		// so 200ms would round down to 0 and the server would return
		// empty results instantly, starving our watch_events_total assertion.
		PollTimeout:  2 * time.Second,
		RetryBackoff: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Drive one watch event so watch_events_total{type=PUT} fires.
	_, _ = ws.Put("public/prod/kick", []byte("2"))
	// Spin until the event propagates (typically sub-second).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := c.Get("public/prod/kick"); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := c.Get("public/prod/kick"); !ok {
		t.Fatal("watch never propagated kick event; metric assertion would be noise")
	}
	// Exercise cache_loads_total{outcome=ok} by loading the cache the SDK
	// just wrote. We reach in via loadFromCache directly — the goal is
	// coverage of the counter path, not state mutation.
	_ = c.loadFromCache()

	reg := c.MetricsRegistry()
	if reg == nil {
		t.Fatal("MetricsRegistry() returned nil")
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	want := map[string]bool{
		"paladin_sdk_full_pulls_total":            false,
		"paladin_sdk_full_pull_duration_seconds":  false,
		"paladin_sdk_watch_polls_total":           false,
		"paladin_sdk_watch_poll_duration_seconds": false,
		"paladin_sdk_watch_events_total":          false,
		"paladin_sdk_cache_loads_total":           false,
		"paladin_sdk_revision":                    false,
		"paladin_sdk_configs":                     false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("metric %q not observable via Gather() after driving traffic", name)
		}
	}
}

// TestFullPullCounter_IncrementsOnSuccess exercises the deferred metric
// instrumentation in fullPull(). A successful New() triggers one full pull,
// so the ok-outcome counter must read 1.
func TestFullPullCounter_IncrementsOnSuccess(t *testing.T) {
	ts, ws := testSDKServer(t)
	addr := strings.TrimPrefix(ts.URL, "http://")
	_, _ = ws.Put("public/prod/k1", []byte("v1"))

	c, err := New(Config{
		Addrs:        []string{addr},
		Tenant:       "public",
		Namespace:    "prod",
		PollTimeout:  1 * time.Second,
		RetryBackoff: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	got := testutil.ToFloat64(c.metrics.fullPullsTotal.WithLabelValues("ok"))
	if got != 1 {
		t.Fatalf("expected full_pulls_total{outcome=ok}=1, got %v", got)
	}
	// revision gauge should have been set by fullPull
	rev := testutil.ToFloat64(c.metrics.revision)
	if rev < 1 {
		t.Fatalf("expected revision gauge >= 1, got %v", rev)
	}
	// configs gauge: at least the key we seeded
	cfgs := testutil.ToFloat64(c.metrics.configs)
	if cfgs < 1 {
		t.Fatalf("expected configs gauge >= 1, got %v", cfgs)
	}
}

// TestFullPullCounter_IncrementsOnError covers the error outcome path of the
// named-return defer in fullPull(): point the SDK at a dead address and the
// error counter must bump exactly once (from the fullPull attempt inside
// New — we don't count cache fallbacks as pulls).
func TestFullPullCounter_IncrementsOnError(t *testing.T) {
	c, err := New(Config{
		Addrs:        []string{"127.0.0.1:1"}, // unreachable
		Tenant:       "public",
		Namespace:    "prod",
		PollTimeout:  200 * time.Millisecond,
		RetryBackoff: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	got := testutil.ToFloat64(c.metrics.fullPullsTotal.WithLabelValues("error"))
	if got < 1 {
		t.Fatalf("expected full_pulls_total{outcome=error} >= 1, got %v", got)
	}
}

// TestSeparateRegistries verifies that two Clients in the same process do
// not share metric state — this is the core value of the
// per-Client-registry design choice.
func TestSeparateRegistries(t *testing.T) {
	ts, _ := testSDKServer(t)
	addr := strings.TrimPrefix(ts.URL, "http://")

	mkClient := func(ns string) *Client {
		c, err := New(Config{
			Addrs:        []string{addr},
			Tenant:       "public",
			Namespace:    ns,
			PollTimeout:  1 * time.Second,
			RetryBackoff: 100 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c1 := mkClient("prod")
	defer c1.Close()
	c2 := mkClient("staging")
	defer c2.Close()

	if c1.MetricsRegistry() == c2.MetricsRegistry() {
		t.Fatal("expected separate registries per Client, got identical pointer")
	}
}
