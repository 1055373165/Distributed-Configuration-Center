<div align="center">

# PaladinCore

**A Raft-based distributed configuration center in ~2,000 lines of Go — built to be read, not just run.**

[![Go Version](https://img.shields.io/badge/go-1.23%2B-00ADD8?logo=go)](https://go.dev/)
[![Raft](https://img.shields.io/badge/consensus-hashicorp%2Fraft-5D3FD3)](https://github.com/hashicorp/raft)
[![Storage](https://img.shields.io/badge/storage-bbolt-4B8BBE)](https://github.com/etcd-io/bbolt)
[![Status](https://img.shields.io/badge/status-educational%20%2F%20alpha-orange)](#project-status)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](#license)

*etcd's core ideas, Consul's leader-forwarding, Ctrip Paladin's SDK lifecycle — distilled to something you can read in a weekend.*

[**Quickstart**](#quickstart) · [**Architecture**](#architecture) · [**API**](#http-api) · [**SDK**](#go-sdk) · [**7-Day Study Guide**](#7-day-study-guide) · [**Design Decisions**](#design-decisions)

</div>

---

## Why this exists

Most distributed-systems learning material lives at one of two extremes: a 3-line blog post that explains Raft with a paxos analogy and helps no one, or the 200k-line etcd source tree that helps only the 1% willing to spelunk. **PaladinCore sits in the middle.** It is a working, testable, 3-node-clusterable configuration center — revision semantics, watch long-polling, FSM-replicated writes, transparent leader forwarding, graceful SDK degradation — all in code small enough to read end-to-end in an afternoon.

It is not a production config center. It is the **reference implementation** you wish existed while you were studying etcd, Consul, or your company's in-house config service. Every module is annotated with the real-world systems it borrows from and the interview-style trade-off that shaped it.

### What makes it worth your time

- **Read-sized.** ~2,000 lines total, including tests. Every file answers one question.
- **Etcd-compatible mental model.** Global monotonic `Revision`, per-key `CreateRevision` / `ModRevision` / `Version`, prefix `List`, long-poll `Watch`. If you understand etcd's v3 semantics, you already understand PaladinCore.
- **Real Raft.** Backed by [`hashicorp/raft`](https://github.com/hashicorp/raft), not a toy. You get log replication, leader election, snapshots, and log compaction — the same building blocks used by Consul and Nomad.
- **Honest about its limits.** See [Non-Goals](#non-goals). No TLS, no RBAC, no gRPC, no multi-DC. You get clarity, not breadth.
- **Curriculum, not just a repo.** Seven chronological `docs/day{1..7}.md` walk the exact same code from empty-directory to 3-node cluster, one commit per layer of complexity.

---

## Project status

> **⚠️ Educational / alpha.** This is a teaching artifact. It has tests and runs correctly in the documented paths, but it has **no production hardening** — no auth, no TLS, no rate limiting, no multi-DC replication, no chaos-tested failure recovery. Do not put it on the internet. Do run it on your laptop, break it, read the code, and learn how a real config center works.

---

## Features

| Capability                 | How it's built                                              | Inspired by                |
| -------------------------- | ----------------------------------------------------------- | -------------------------- |
| Versioned KV store         | BoltDB single-writer + monotonic global `Revision` counter  | etcd v3 MVCC (simplified)  |
| Multi-tenant namespaces    | K8s-style paths: `/{tenant}/{namespace}/{name}`             | Kubernetes resource paths  |
| Watch / change notification| In-memory ring buffer + `sync.Cond` + HTTP long polling     | etcd `watchableStore`      |
| Distributed consensus      | `hashicorp/raft` FSM wrapping the local store               | etcd, Consul, Nomad        |
| Transparent write routing  | Follower proxies writes to Leader via HTTP forwarding       | Consul's `ForwardRPC`      |
| Cluster membership         | `POST /admin/join` → `raft.AddVoter`; peer-HTTP replicated  | Raft joint consensus       |
| Log compaction / snapshots | `FSM.Snapshot` every 1,024 entries; auto-install on laggers | Raft paper §7              |
| Go SDK                     | Full pull → long-poll loop → SHA-256-checksummed local cache| Ctrip Paladin SDK V2       |
| CLI + standalone + cluster | Single binary, three modes (`put`/`serve`/`cluster`)        | etcdctl + etcd             |
| 3-node deploy              | Docker Compose **and** pure-bash `scripts/cluster-local.sh` | —                          |

---

## Quickstart

### 60 seconds, one machine, no Docker

```bash
git clone <your-fork-url> paladin-core && cd paladin-core
go build -o paladin-core ./cmd/paladin-core
./scripts/cluster-local.sh --fresh          # starts 3 nodes on :8080/:8081/:8082
```

You now have a live 3-node Raft cluster. Exercise it:

```bash
# Write through node2 (a Follower) — PaladinCore transparently forwards to the Leader
curl -X PUT http://127.0.0.1:8081/api/v1/config/public/prod/db_host -d '10.0.0.1'

# Read from node3 (local, stale-OK read)
curl http://127.0.0.1:8082/api/v1/config/public/prod/db_host

# Inspect cluster state
curl -s http://127.0.0.1:8080/admin/stats | jq
#  => {"state": "Leader", "term": "2", "commit_index": "5", ...}

# Kill the leader and watch the cluster heal in ~2s
kill $(sed -n 1p .cluster-pids)
sleep 3
curl -s http://127.0.0.1:8081/admin/stats | jq .state   # "Leader"

# Stop everything
./scripts/cluster-stop.sh --clean
```

### Docker Compose

```bash
docker compose up -d
# node1 → localhost:8080  node2 → :8081  node3 → :8082
docker compose logs -f
docker compose down -v
```

### Standalone (single node, no Raft)

```bash
./paladin-core serve :8080
# Or as a CLI over the local BoltDB:
./paladin-core put public/prod/db_host 10.0.0.1
./paladin-core get public/prod/db_host
./paladin-core list public/
```

---

## Architecture

PaladinCore stacks five thin layers. Each layer has one job and exactly one upstream consumer, so you can learn it bottom-up and the arrows never cross.

```text
 ┌──────────────────────────────────────────────────────────────┐
 │  SDK (sdk/client.go)                                         │
 │    full pull  →  long-poll watch loop  →  local cache        │
 └───────────────────────┬──────────────────────────────────────┘
                         │  HTTP
 ┌───────────────────────┴──────────────────────────────────────┐
 │  HTTP API layer (server/)                                    │
 │    routing, path parsing, forward-to-leader, watch handler   │
 └───────────────────────┬──────────────────────────────────────┘
                         │  node.Apply(op)       (write path)
                         │  node.Get/List        (read path, local)
 ┌───────────────────────┴──────────────────────────────────────┐
 │  Raft layer (raft/node.go)                                   │
 │    hashicorp/raft + FSM.Apply  ⇄  peers (AppendEntries)      │
 └───────────────────────┬──────────────────────────────────────┘
                         │  store.Put / store.Delete
 ┌───────────────────────┴──────────────────────────────────────┐
 │  WatchableStore (store/watchable.go)                         │
 │    every write → BoltStore + WatchCache.Append               │
 └───────┬───────────────────────────────────┬──────────────────┘
         │                                   │
 ┌───────┴────────────┐            ┌─────────┴───────────────────┐
 │ BoltStore          │            │ WatchCache (ring buffer)    │
 │ bbolt, revision++  │            │ sync.Cond, Broadcast() wakes│
 │ per-write          │            │ blocked long-poll clients   │
 └────────────────────┘            └─────────────────────────────┘
```

### D2 diagram source

![alt text](resources/d2%20(3).svg)

### Module map

| Package               | Purpose (≤ 1 sentence)                                                    | LoC  |
| --------------------- | ------------------------------------------------------------------------- | ---- |
| `store/`              | Versioned KV (`BoltStore`) + `WatchableStore` + `WatchCache` ring buffer  | ~450 |
| `raft/`               | `raft.Node` wrapping `hashicorp/raft`, with FSM + snapshot + peer HTTP    | ~280 |
| `server/`             | HTTP router, config handler, watch long-poll, leader-forward, admin API  | ~430 |
| `sdk/`                | Go client: full pull → watch loop → SHA-256-verified local cache         | ~260 |
| `cmd/paladin-core/`   | CLI entry point with `serve` / `cluster` / `put\|get\|delete\|list\|rev` | ~160 |

---

## HTTP API

Paths follow the Kubernetes-style hierarchy `/{tenant}/{namespace}/{name}`.

### Configuration

| Method | Path                                              | Description                                               |
| ------ | ------------------------------------------------- | --------------------------------------------------------- |
| `PUT`  | `/api/v1/config/{tenant}/{ns}/{name}`             | Create or update. Body is raw value (≤ 1 MiB). Write goes through Raft; followers auto-forward to Leader. |
| `GET`  | `/api/v1/config/{tenant}/{ns}/{name}`             | Single read. Local, stale-OK.                             |
| `GET`  | `/api/v1/config/{tenant}/{ns}/`                   | Prefix list.                                              |
| `GET`  | `/api/v1/config/{tenant}/`                        | Tenant-wide prefix list.                                  |
| `DELETE` | `/api/v1/config/{tenant}/{ns}/{name}`           | Delete (bumps revision, emits watch event).               |
| `GET`  | `/api/v1/rev`                                     | Current global revision.                                  |

### Watch

| Method | Path                                                 | Query params                     |
| ------ | ---------------------------------------------------- | -------------------------------- |
| `GET`  | `/api/v1/watch/{tenant}/{ns}/`                       | `revision=N` (last seen), `timeout=30` (max 60). Blocks until events with `rev > N` exist or timeout. |

### Admin

| Method | Path                                                      | Description                                                   |
| ------ | --------------------------------------------------------- | ------------------------------------------------------------- |
| `POST` | `/admin/join?id=NODE&addr=RAFT_ADDR&http=HTTP_ADDR`       | Register a new voter (must be sent to the Leader).            |
| `POST` | `/admin/leave?id=NODE`                                    | Remove a voter (Leader only).                                 |
| `GET`  | `/admin/stats`                                            | Raft state (term, commit_index, applied_index, is_leader, …). |
| `GET`  | `/healthz`                                                | Liveness probe.                                               |

### Response shape

```json
{
  "revision": 17,
  "count": 1,
  "configs": [
    {
      "key": "public/prod/db_host",
      "value": "10.0.0.1",
      "revision": 17,
      "create_revision": 12,
      "mod_revision": 17,
      "version": 4
    }
  ]
}
```

`X-Paladin-Revision` is also returned as an HTTP header on every write, so clients can pipeline reads without waiting on the body.

---

## Go SDK

```go
import "paladin-core/sdk"

c, err := sdk.New(sdk.Config{
    Addrs:     []string{"localhost:8080", "localhost:8081", "localhost:8082"},
    Tenant:    "public",
    Namespace: "prod",
    CacheDir:  "/var/cache/myapp",   // optional; enables fallback on server outage
    PollTimeout:  30 * time.Second,  // long-poll window, capped at 60s server-side
    RetryBackoff: 1 * time.Second,
})
if err != nil { /* handle */ }
defer c.Close()

// Synchronous lookup of the last-known value
if v, ok := c.Get("public/prod/db_host"); ok {
    fmt.Println(string(v))
}

// React to changes. key="" subscribes to all keys in the namespace.
c.OnChange("public/prod/db_host", func(key string, old, new []byte) {
    log.Printf("rotated: %s: %q -> %q", key, old, new)
})
```

Behavior contract:

1. `New` does one synchronous full pull. If that fails and `CacheDir` is set, the cache is loaded and SHA-256-verified; corrupt caches are rejected.
2. A background `watchLoop` long-polls forever. It survives server restarts, DNS flaps, and serialization errors with constant-backoff retries.
3. `Close` cancels the context and waits for all in-flight callbacks to finish — no zombie goroutines.

---

## Design decisions

Every decision here was consciously made in favor of **readability over flexibility**. That is the whole point of this repo.

| # | Decision                     | Alternative considered              | Why we chose this                                                                 | Cost we accept                                  |
| - | ---------------------------- | ----------------------------------- | --------------------------------------------------------------------------------- | ----------------------------------------------- |
| 1 | BoltDB for KV + Raft log     | RocksDB, Pebble, LevelDB            | Single-writer model matches Raft's "only leader writes" invariant; zero-CGO build | No compression, slower writes on huge values    |
| 2 | `hashicorp/raft` v1          | Hand-rolled Raft, `etcd/raft`       | Battle-tested in Consul/Nomad; dramatically smaller surface area than `etcd/raft` | Less flexible pipelining; no pre-vote by default |
| 3 | HTTP/JSON API                | gRPC + protobuf                     | Zero codegen, `curl`-debuggable, obvious wire format                              | ~2–3× higher RPC overhead than gRPC             |
| 4 | Long polling for watch       | WebSocket, gRPC server-streaming    | No stateful connection, no reconnection logic, works through any HTTP proxy       | Extra handshake per poll; 30s cadence ceiling   |
| 5 | Ring buffer for events       | Full append-only log                | O(1) append, bounded memory, matches etcd's `watchableStore`                      | Slow watchers that fall behind `capacity=4096` get evicted silently |
| 6 | Stale reads by default       | Linearizable reads via `ReadIndex`  | Reads serve from any node's local bbolt — cheap and horizontally scalable          | Up to `apply lag` ms of staleness (typically ms) |
| 7 | Leader-forward over HTTP     | Client-side leader discovery        | Client stays topology-ignorant; one URL works forever                             | One extra hop per write                          |
| 8 | Peer HTTP addresses in store | External service-discovery (Consul) | `LeaderHTTPAddr()` survives failover because it reads replicated state             | Bootstraps a chicken-and-egg on first leader    |
| 9 | FSM `Apply` runs in BoltDB   | Separate state machine + WAL        | Raft-log's ACK already guarantees durability; re-WALing wastes IOPS                | A bbolt write error stalls the Raft apply loop  |

Each decision is **reversible** — every module has a narrow interface (`store.Store`, `raft.FSM`, etc.), so a future iteration can swap any one piece without touching the others.

---

## Non-Goals

**Explicitly out of scope.** These are great features. They are also how learning projects become unfinished production projects. PaladinCore will stay small.

- ❌ **TLS / mTLS** — run it inside a VPC or behind a reverse proxy.
- ❌ **Authentication / RBAC** — no JWT, no ACLs, no audit log.
- ❌ **Multi-datacenter / cross-region replication** — one Raft group, one region.
- ❌ **Linearizable reads (`ReadIndex`)** — stale-by-a-few-ms reads only. A `?consistent=true` flag is sketched but not implemented.
- ❌ **Transactions / compare-and-swap** — no `Txn`, no `If/Then/Else` etcd-style.
- ❌ **Leases / TTLs** — keys live forever unless deleted.
- ❌ **gRPC API** — HTTP only.
- ❌ **Web UI / dashboard** — `curl` + `jq` is the UI.
- ❌ **Horizontal scaling of the write path** — one Leader, full stop.
- ❌ **Backup / restore tooling** — `cp data-node1/data.db` is the backup strategy.

If you need any of these, use [etcd](https://etcd.io), [Consul](https://www.consul.io), [Apollo](https://github.com/apolloconfig/apollo), or [Nacos](https://nacos.io).

---

## 7-Day Study Guide

The repository was built one day at a time. Each `docs/dayN.md` (中文) walks the exact diff that arrived that day, the design question it answers, and the interview-grade "soul-searching" (灵魂拷问) it exposes.

| Day | You build                              | Code added                                       | Interview concept                          | Doc                              |
| --- | -------------------------------------- | ------------------------------------------------ | ------------------------------------------ | -------------------------------- |
| 1   | BoltDB KV with revision semantics      | `store/store.go`, `store/bolt.go`                | Logical vs physical clocks, TX atomicity   | [day1.md](docs/day1.md)          |
| 2   | HTTP API + multi-tenant paths          | `server/server.go`                               | RESTful design, K8s-style resource paths   | [day2.md](docs/day2.md)          |
| 3   | Watch: ring buffer + long polling      | `store/watch.go`, `server/watch.go`              | `sync.Cond`, long-poll vs WebSocket vs SSE | [day3.md](docs/day3.md)          |
| 4   | Raft FSM replacing the local store     | `raft/node.go`                                   | Consensus, log replication, snapshotting   | [day4.md](docs/day4.md)          |
| 5   | Follower → Leader forwarding           | `server/raft_server.go`                          | Transparent RPC proxying, read consistency | [day5.md](docs/day5.md)          |
| 6   | SDK: pull + watch + checksummed cache  | `sdk/client.go`                                  | Graceful degradation, bounded lifetimes    | [day6.md](docs/day6.md)          |
| 7   | 3-node deploy + cluster admin          | `Dockerfile`, `docker-compose.yml`, `cmd/`       | Node lifecycle, leader failover            | [day7.md](docs/day7.md)          |

A good "first read" order for the source:

```text
store/store.go    →  store/bolt.go       →  store/watch.go
   →  store/watchable.go  →  server/server.go  →  server/watch.go
      →  raft/node.go     →  server/raft_server.go
         →  sdk/client.go →  cmd/paladin-core/main.go
```

---

## Development

### Build & test

```bash
# Build the single binary
go build -o paladin-core ./cmd/paladin-core

# Run the full test suite (unit + integration)
go test ./...

# Race-detector pass
go test -race ./...

# One package, verbose
go test -v ./raft/...
```

Tests cover: `BoltStore` CRUD and revision invariants, `WatchCache` concurrent readers, HTTP handler contract, long-poll wake semantics, and a 3-node in-process Raft integration test.

### Repository layout

```text
paladin-core/
├── cmd/paladin-core/       CLI entry point (serve | cluster | put/get/...)
├── store/                  versioned KV + watch ring buffer
├── raft/                   hashicorp/raft wrapper + FSM + snapshot
├── server/                 HTTP layer + leader forwarding + admin
├── sdk/                    Go client with cache fallback
├── scripts/                cluster-local.sh / cluster-stop.sh
├── docs/                   day{1..7}.md learning curriculum (中文)
├── Dockerfile              multi-stage, CGO-free build
└── docker-compose.yml      3-node local cluster
```

---

## Failure modes & how to trigger them

Part of learning is breaking things on purpose. Reproduce these in 30 seconds each:

| Scenario                            | How to trigger                                                          | Expected behavior                                                          |
| ----------------------------------- | ----------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| Leader failover                     | `kill $(sed -n 1p .cluster-pids)` then watch `/admin/stats` on node2/3  | New leader elected within ~2s; writes resume                               |
| Follower write                      | `curl -X PUT http://:8081/...`                                          | 201/200 — forwarded to leader transparently                                |
| Stale read window                   | Write on leader, immediately read on followers                          | Read may miss the latest write by ≤ apply-latency (typically sub-ms)       |
| SDK survives outage                 | Start SDK, `./scripts/cluster-stop.sh`, keep calling `c.Get`            | Reads continue from the SHA-256-verified local cache                       |
| Watcher falling behind              | Stall an SDK > 4,096 events worth of writes                             | Oldest events evicted from ring buffer; SDK must do a full re-pull         |
| Corrupt local cache                 | `echo garbage > /var/cache/myapp/paladin_public_prod.json`              | SDK rejects the cache on checksum mismatch, returns startup error          |
| `bbolt` lock contention on restart  | Start two standalone binaries against the same `paladin-core.db`        | Second one blocks on bbolt file lock — expected                            |

---

## Roadmap

Small, intentional, non-committal:

- [ ] `?consistent=true` linearizable reads via `raft.VerifyLeader`
- [ ] Batched writes (`raft.ApplyBatch`)
- [ ] Prometheus `/metrics` endpoint with Raft + store histograms
- [ ] Go SDK: round-robin + health-aware endpoint selection (currently uses `Addrs[0]`)
- [ ] Java / Python SDK mirrors with identical wire contract
- [ ] Chaos test suite (`jepsen`-flavored, in-repo)

If you would like any of these and are willing to contribute, open a discussion first.

---

## Contributing

PRs welcome, especially those that **make the code smaller or clearer** without removing semantics. Keep in mind:

1. **Readability is the first-class goal.** An optimization that adds 100 lines to save 5µs will usually be declined.
2. **One module, one responsibility.** Follow the existing package boundaries.
3. **Every new public function gets a `// Why:` comment** explaining the design choice, not just the "what".
4. **Tests are required** for new behavior. Race-detector must stay green.

Before you send a PR: `go vet ./... && go test -race ./...`.

---

## Acknowledgments

PaladinCore is a loving homage. It would not exist without:

- **[etcd](https://etcd.io/)** — source of the revision model, the watch semantics, and the `watchableStore` design.
- **[HashiCorp Raft](https://github.com/hashicorp/raft)** — the only reason the consensus layer fits in 280 lines.
- **[bbolt](https://github.com/etcd-io/bbolt)** — single-writer B+tree KV, perfect fit for Raft.
- **[Consul](https://www.consul.io/)** — the `ForwardRPC` pattern used in `forwardToLeader`.
- **Ctrip Paladin** — the three-phase SDK lifecycle (pull → watch → shutdown with cache fallback).
- **[Diego Ongaro's Raft paper](https://raft.github.io/raft.pdf)** — the clearest systems paper of the last decade.

---

## License

Released under the [MIT License](LICENSE). Use it freely for learning, teaching, internal tooling, and interview prep. **Do not use it as-is in production.** If you want a production-grade version, fork it and start by crossing items off [Non-Goals](#non-goals).

---

<div align="center">
  <sub>Built as a 7-day exercise in turning papers into running code. If it helped you understand distributed systems a little better, that was the point.</sub>
</div>
