# P0 — Raft Write RPS Ceiling (OV1)

**Status**: Laptop baseline complete. Linux VM follow-up **required before P1 kickoff**.
**Generated**: 2026-04-22
**Sweep artefact**: `bench/results/20260422-104733_*.json`

## 目的

回答 eng-review OV1 的问题：**在不改动任何代码的情况下，当前 `hashicorp/raft` + `bbolt` + `fsync=on` 组合的写 RPS 物理极限是多少？** 作为 §五 KPI 表的可达性锚点，避免把"理论值"当"目标值"。

## 环境

| 项目 | 值 |
|---|---|
| Host | `MBP-H9KPMJHHG6-0644.local` |
| OS / Arch | `darwin/arm64` (**不是 Linux VM**) |
| CPUs | 12 (GOMAXPROCS=12) |
| Go | `go1.23.3` |
| Cluster | 3 nodes localhost, HTTP `:18080-18082`, Raft `:19001-19003` |
| fsync | `on` (bbolt default) |
| FS | APFS (laptop SSD) |

**诚实免责**：macOS APFS fsync 典型 **10–30ms**（Apple `F_FULLSYNC` 极慢）；Linux NVMe fsync 典型 **100µs–1ms**。**laptop 绝对数字不能当 P2 KPI 锚点**；它只能用来讨论**曲线形状**（OV2）。Linux VM 必须复测后才能把 §五 KPI 表确认/下修。

## 测量

```bash
HTTP_BASE=18080 RAFT_BASE=19001 ./scripts/cluster-local.sh --fresh
ADDRS=127.0.0.1:18080,127.0.0.1:18081,127.0.0.1:18082 ./scripts/bench-suite.sh
```

Sweep: `CONCS="1 8 32 128"`, `DURATION=30s`, `WARMUP=3s`, value 64B。

## 结果（write_only）

| conc | RPS | p50 | p95 | p99 | p99.9 | SLO `p99≤50ms` 合规？ |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | **37** | 25ms | 38ms | 68ms | 218ms | ❌（p99=68ms） |
| 8 | **65** | 114ms | 179ms | 229ms | 745ms | ❌ |
| 32 | 63 | 457ms | 699ms | 1.32s | 1.60s | ❌ |
| 128 | 67 | 1.78s | 2.04s | 2.12s | 2.14s | ❌ |

**关键观察**：

1. **写 throughput ceiling ≈ 65 RPS**。从 c=8 → c=128 并发 16x，RPS 基本持平（65 → 67），latency 从 229ms 涨到 2.12s。典型 **queueing collapse**——hashicorp/raft 的 wire-level pipelining 在 c=8 附近饱和。
2. **c=1 的 mean 27ms ≈ 一次 fsync + Raft commit 的总开销**。min=13.6ms 说明 laptop APFS fsync 底 ~15ms，和 Apple `F_FULLSYNC` 特性一致。
3. **在严格 `p99≤50ms` SLO 下，laptop 没有合规的 RPS 点**。c=1 都超。说明要么 SLO 要放宽到 laptop 不适合测 SLO，要么必须上 Linux。
4. 现有 spec §五 基线值 "写 RPS ~55 @ p99 523ms" 的 523ms 是 queueing collapse 区间的数（大概 c=4–8 未 SLO 约束时的值），**不能用来和"目标 3000 @ p99≤50ms"做 apples-to-apples 对比**。

## Linux 估算（非权威，仅供 P2 KPI 预判）

假设 Linux NVMe fsync ~1ms（laptop 的 ~15x），其余开销（Go 调度、net 栈、bbolt mmap）在 Linux 上也快 2–3x：

| 项 | Laptop 实测 | Linux 估算 | 方法 |
|---|---:|---:|---|
| write c=1 RPS | 37 | **~300–500** | fsync scale factor |
| write knee RPS (c=? @ p99≤50ms) | 0 合规 | **~500–1000** | 需 Linux 复测确认 knee 位置 |
| write fsync 单次耗时 | ~15ms | ~1ms | APFS vs NVMe 已知差距 |

**P2 KPI 评估**（目标：写 RPS @ conc=64 @ p99≤50ms ≥ 3000）：

- 若 Linux 裸测 c=8 knee 是 ~800 RPS（估算中位），BatchProposer 典型 5–8x 收益 → **P2 可能做到 4000–6400 RPS**。目标 3000 **理论可达**。
- 若 Linux 实测 knee 比估算低一半（即 ~400 RPS），BatchProposer 5–8x → **2000–3200 RPS**。目标 3000 **紧绷**，可能需下调到 2500 或放宽 SLO。

## 建议

1. **P2 KPI 不动**（目标 3000 保留），但在 P1 kickoff 前用 **docker-compose 跑一次 Linux baseline**，把 spec §五 "基线" 列替换成 Linux 数字。
2. **§五 KPI 表里"基线 ~55"这一格需加脚注**：该值来自无 SLO 约束下的历史测量，与"目标 3000 @ p99≤50ms"不严格可比；Linux 复测后用 SLO-constrained 数字替换。
3. **若 Linux 实测 c=8–32 RPS < 500**，P2 目标应下调到 2500 或改 SLO 到 `p99≤100ms`，不强行虚报 3000。
4. **保留本 laptop baseline 作 regression guard**：P1/P2 在 laptop 上再跑同 sweep，曲线形状不许劣化（绝对值允许浮动）。

## Raw 数据

- `bench/results/20260422-104733_write_only_c1.json`
- `bench/results/20260422-104733_write_only_c8.json`
- `bench/results/20260422-104733_write_only_c32.json`
- `bench/results/20260422-104733_write_only_c128.json`
- 汇总：`bench/results/20260422-104733_report.md`

## Follow-up

- [ ] Linux VM 复测同 sweep，填入本文件"Linux 实测"新表格。
- [ ] 更新 `docs/paladincore-v2-spec.md` §五 基线列脚注。
- [ ] 若必要，在 P1 kickoff 前重议 P2 的 3000 RPS 目标。
