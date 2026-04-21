# TODOS — PaladinCore

This file tracks scope items explicitly deferred from the active plan (`docs/paladincore-v2-spec.md`). Each item records why it was deferred, the trigger for re-evaluation, and the source of the decision.

Keep this file short. When an item becomes active, move it into the relevant spec and delete from here.

---

## Deferred from v2 CEO review (2026-04-21)

### Multi-Raft sharding (experimental branch)

- **Why deferred**: Violates the `lightweight` north star; estimated +1500 LOC would break the 6000 cap; current write throughput ceiling (~3k/s) is acceptable for typical config-center workloads (Apollo / Nacos production writes usually < 500/s).
- **Source**: `docs/paladincore-v2-spec.md` §9.1; CEO plan `~/.gstack/projects/paladin-core/ceo-plans/2026-04-21-paladincore-v2.md` E4 = C.
- **Trigger to re-evaluate**:
  1. A real user reports sustained write RPS > 10k for > 1 week; OR
  2. Post-v2 perf profiling shows write path bottleneck is Raft protocol itself (not bbolt/fsync).
- **If revisited**: produce ADR first (5 days) before any code; experimental branch only; do not merge to main without a fresh CEO review.

### Public continuous benchmark dashboard

- **Why deferred**: v2 stance is `rigor over marketing`. Public Pages is a marketing artifact; maintenance overhead (CI tokens, deploy triggers, retention) distracts from core work. Early numbers during P0–P2 would be unflattering.
- **Source**: `docs/paladincore-v2-spec.md` §9.2; CEO plan E5 = C.
- **Trigger to re-evaluate**: Project gains meaningful traction — GitHub stars > 1000 OR external contributors ≥ 5. At that point the signal-to-maintenance ratio may flip.
- **Alternative in place**: `bench/baselines/` committed JSON + per-PR CI diff; anyone who wants a deep dive can `git log bench/`.

### Full Jepsen (Clojure) integration

- **Why deferred**: Brings JVM + Clojure dependency; breaks Go-native lightweight; raises new-contributor barrier. `porcupine + toxiproxy` covers ~80% of Jepsen's value in Go-native form.
- **Source**: `docs/paladincore-v2-spec.md` §9.3 and decision 10; CEO plan decision D-2 = A.
- **Trigger to re-evaluate**: A specific bug class is found that porcupine cannot detect but Jepsen can; OR the project formally courts enterprise adoption where the Jepsen brand is a contractual requirement.

### Full 12–15 article technical series (P7 extended)

- **Why deferred**: CEO review chose the 5-article focused set (workload shape / Raft choice / delay decomp / group commit / fsync-dominated) over the full 12–15 article series. The remaining 7–10 topics are valuable but not v2-critical.
- **Source**: CEO plan E2 = B.
- **Trigger to re-evaluate**: After v2 ships and the 5 articles have organic traction, consider extending with the remaining topics (watch fanout, ReadIndex, bench methodology deep-dive, failure mode studies, etc.).

---

## Open questions from eng review (2026-04-21)

These are outside-voice concerns raised during eng review that were **not** inline-decided. They deserve revisiting before P6/P8 kickoff.

### OV3: crash-consistency / fsync-ordering bugs are invisible to porcupine

- **What**: porcupine verifies history ordering (linearizability), NOT durability under crash. A bug where ack returns before fsync completes would pass every chaos scenario but lose data in production.
- **Why it matters**: The spec's "no data loss" narrative is unearned with current chaos toolkit.
- **Options when revisited**:
  1. Add syscall-level fsync-injection (LD_PRELOAD hook in `chaos/injector/`).
  2. Adopt a crash-consistency tool like ALICE or CrashMonkey (Go bindings may not exist → maintenance cost).
  3. Narrow the claim: spec says "linearizability verified" only, drop "no data loss" from marketing.
- **Trigger to re-evaluate**: Before writing v1.0 release notes or `docs/chaos-report-v1.md`.

### OV4: gRPC Watch (P8) may not be worth 10 person-days + 1000 LOC

- **What**: Spec's P8 adds a parallel transport path for 30ms tail vs HTTP's 60ms. For config center users (5s poll intervals typical), the tail-latency win is not user-visible. Cost: perpetual dual-transport SDK maintenance, protoc toolchain dependency, chaos scenarios × 2.
- **Why deferred here**: Scope already debated in CEO review and locked. But OV concern is substantive — worth revisiting after P0 baseline measurement (maybe HTTP long-poll p99 is already <50ms on current stack, which would collapse P8's case).
- **Trigger to re-evaluate**: End of P0. If HTTP long-poll p99 < 40ms on 3-node Linux cluster, reconsider whether P8's 30ms target is worth the cost.

### OV9: "configuration center" positioning may be a trap

- **What**: Spec comparators are Apollo/Nacos. Those products have rich feature trees (grayscale rollout, environments, audit logs, UI) that v2 does not address. Positioning v2 as "configuration center" invites losing comparisons on every non-perf axis.
- **Alternative positioning**: "a tiny, auditable, linearizable K/V store suitable for configuration workloads". Narrower, more honest.
- **Trigger to re-evaluate**: Before P4 (benchmark release) README headline update. Get the positioning right before publishing numbers under any banner.

### OV10: 66 person-days (~4 months) for a pre-v1 OSS project is high-risk

- **What**: Spending 4 months on rigor before any user adoption signal. If no one cares, rigor didn't matter. Alternative: ship a narrower v1 (P0+P1+P2+docs) in 6 weeks, see if anyone appears, then decide on P6/P8.
- **Counter**: The project's stated character is "rigor over marketing" — shipping fast-but-unverified would violate the north star. Accepting this counter by default (no action needed unless you want to revisit).
- **Trigger to re-evaluate**: After P4 release if v0.9 preview gets zero external interest in 4 weeks.
