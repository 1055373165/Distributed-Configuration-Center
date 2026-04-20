# PaladinCore Benchmark Toolkit

A small, opinionated load-testing toolkit for PaladinCore. Built **inside the
repo** (no external test rig) so benchmarks are versioned with the code and
anyone can reproduce a number on their laptop in 30 seconds.

## Why build our own?

Off-the-shelf HTTP benchmark tools (`wrk`, `vegeta`, `hey`) work, but they
don't speak the Paladin wire contract natively and they don't know how to:

- Prepopulate keys on the leader and wait for follower replication.
- Fan reads across every cluster member and writes at the leader.
- Emit a stable JSON result that diffs cleanly against a baseline.
- Filter warm-up samples so GC ramp-up and bbolt page-faults don't pollute
  steady-state numbers.

A 400-line Go toolkit gets us all of that, re-uses `net/http`, and compiles
into one binary.

---

## Design principles

### 1. Per-worker histograms, merged at the end

The single biggest measurement artefact in high-concurrency benchmarks is
histogram lock contention. We avoid it by giving each worker its own `Hist`,
writing to it lock-free on the hot path, and merging at the end.

### 2. Warm-up by timestamp, not by reset

We don't reset state after warm-up (that would race with running workers).
Instead, each worker compares its request's timestamp against
`measureStart := startedAt + WarmUp` and silently drops samples older than
that. Simple, lock-free, and correct.

### 3. SLO-driven, not peak-driven

A single "max QPS" number is misleading — every system can be pushed past
its SLO into latency hell. Report the **knee point**: the highest RPS that
still satisfies `p99 ≤ target`. Pick the target (e.g. 50ms) from your
service contract, not from marketing.

### 4. Linear-bucket histograms at 100µs resolution

Range 0–10s, 100_001 buckets, ~800 KB per worker. Enough precision for
HTTP-scale latencies without the complexity of HdrHistogram. Overflow goes
into the last bucket but the exact max is tracked separately so tail
outliers are never silently dropped.

### 5. Environment fingerprint in every result

Every JSON result carries `Env` (Go version, GOMAXPROCS, host, OS, arch,
optional build SHA). Two results are only comparable when their `Env`
matches. This is what separates "we ran a benchmark" from "we did science".

---

## Scenario matrix

Each scenario exercises one measurable aspect of the system:

| Scenario      | What it stresses                                                         | What it tells you                                      |
|---------------|--------------------------------------------------------------------------|--------------------------------------------------------|
| `write_only`  | Raft replication + BoltDB single-writer + fsync                          | **Write ceiling** of the cluster                       |
| `read_only`   | BoltDB stale read + JSON encoding; fans across all nodes                 | Read-side horizontal scalability                       |
| `mixed`       | Configurable R/W ratio (default 95/5)                                    | Realistic configuration-center workload                |

Knobs you'll commonly tune:

- `--concurrency`  — sweep this to find the knee point.
- `--value-size`   — 64 B default (config-like). Try 10 KB / 100 KB / 1 MB.
- `--num-keys`     — 1 key = hot-spot test; 10k keys = replication test.
- `--read-percent` — for `mixed`.

---

## Quickstart

```bash
# 1. Build
go build -o paladin-bench ./cmd/paladin-bench

# 2. Start a 3-node cluster
./scripts/cluster-local.sh --fresh

# 3. Run one scenario
./paladin-bench run --scenario=write_only --concurrency=64 --duration=30s

# 4. Run the canonical sweep (writes JSON + Markdown into bench/results/)
./scripts/bench-suite.sh
```

Sample output:

```text
[bench] scenario=write_only conc=64 duration=30s warmup=3s rps_cap=0 addrs=[127.0.0.1:8080]

scenario   : write_only
duration   : 30.0s
count      : 64281
rps        : 2142.7
errors     : 0  (class: [0 0 64281 0 0 0])
latency    : min=2.1ms mean=29.8ms max=184ms
             p50=28ms p95=61ms p99=104ms p99.9=158ms
```

---

## Finding the knee point

Run the same scenario at increasing concurrency and watch the RPS/latency
trade-off. Define your SLO first (`p99 ≤ 50ms`) — the last concurrency
level that still satisfies it is your **"safe" capacity**.

```bash
for c in 1 4 16 64 256 1024; do
  ./paladin-bench run --scenario=write_only \
    --concurrency=$c --duration=20s --warmup=3s \
    --json=bench/results/knee_c${c}.json
done

./paladin-bench report --in=bench/results --out=knee.md
```

Typical shape on a 3-node localhost cluster (your numbers will differ):

```text
conc=1     rps=330    p99=5ms    ← capacity-starved
conc=16    rps=1900   p99=22ms   ← ramping up
conc=64    rps=2150   p99=110ms  ← knee: latency explodes
conc=256   rps=2180   p99=780ms  ← thrashing
```

The knee is at ~16–64; past it the server queues up work and tail latency
blows up while throughput barely moves. **Your reported "max QPS" should be
the concurrency one step below the knee.**

---

## How to read a result

Every JSON result has three tiers:

```json
{
  "scenario": "write_only",
  "rps": 2142.7,            ← Headline: throughput during measurement window
  "count": 64281,
  "errors": 0,
  "status_class": [0,0,64281,0,0,0],
  "latency": {
    "count": 64281,
    "min_ns": 2100000,
    "mean_ns": 29800000,
    "p50_ns": 28000000,
    "p95_ns": 61000000,
    "p99_ns": 104000000,    ← Headline: your SLO lives here
    "p999_ns": 158000000,   ← Real long-tail (single points of slowness)
    "max_ns": 184000000
  },
  "env": {...}              ← Required for cross-run comparison
}
```

Rules of thumb:

- Compare **p99** across runs, not mean or max.
- If p999 is 10× p99, something is pausing (GC, fsync spike, leader election).
- If RPS ≈ RPSCap but p99 skyrockets, you're overloaded — reduce the cap.
- If errors > 0, the result is **not usable**. Investigate first.

---

## Regression tracking

1. Commit a "known-good" result: `git add bench/baselines/<tag>.json`
2. After changes, re-run the sweep: `./scripts/bench-suite.sh`
3. Diff the Markdown or eyeball the JSON with `jq`:

```bash
diff -u bench/baselines/v0.1.md bench/results/<ts>_report.md
jq '.latency.p99_ns' bench/baselines/v0.1_write_only_c64.json \
                     bench/results/<ts>_write_only_c64.json
```

Set `PALADIN_BUILD=$(git rev-parse --short HEAD)` before running to pin
each result to a specific commit.

---

## Caveats & honest disclaimers

- **Localhost results are upper bounds.** Loopback is sub-µs; real networks
  add 100µs–1ms of RTT that BoltDB fsync dwarfs anyway, but you *must*
  rerun against your target environment before quoting numbers externally.
- **BoltDB fsync dominates writes.** An NVMe SSD fsync is 50–200µs; a
  spinning disk is 5–10ms. If you disable fsync (don't), write RPS goes up
  ~50× and you get to test data loss scenarios.
- **This is closed-loop by default.** It measures "how fast can N clients
  saturate the system?" not "how does the system respond to a fixed
  Poisson arrival rate?" For the latter, the open-loop mode is on the
  roadmap — for now, use `--rps N` to get an approximate rate cap.
- **Coordinated omission is partially mitigated**: warm-up samples are
  dropped, and the rate-limited mode shares a single ticker so slow
  requests don't starve later ones — but if you need strict CO removal
  you'll want a dedicated tool (or wait for open-loop mode).
- **Do not run the client and the server on the same cores.** On a laptop
  this is unavoidable. Reserve at least 2 spare cores; on macOS,
  `sudo renice -n -5 $(pgrep paladin-core)` helps.

---

## Roadmap

- [ ] Open-loop Poisson arrival mode (`--open-loop --rps=N`)
- [ ] `watch_fanout` scenario (N SDK long-polls + low-QPS writes; measures
      E2E notification delay from PUT on leader to `applyEvents` on client)
- [ ] `failover` scenario (kill leader mid-run; measure write-unavailability
      window)
- [ ] Server-side Prometheus scrape on `/metrics` + side-by-side graphs
- [ ] Baseline JSON comparison as a single command (`paladin-bench diff`)
- [ ] Per-request trace export for p99.9 outlier investigation

Contributions welcome — the toolkit is intentionally small.
