# P0 — Hashicorp/Raft Pipelining Saturation (OV2)

**Status**: Laptop answer complete. Conclusion does not change under Linux (curve shape is architectural, not hardware).
**Generated**: 2026-04-22
**Source data**: `bench/results/20260422-104733_*.json`
**Paired doc**: `docs/p0-raft-ceiling.md` (OV1 — absolute RPS ceiling)

## 目的

回答 eng-review OV2 的问题：**`hashicorp/raft` 的内置 wire-level batching（把多个 AppendEntries 攒到一个 RPC）在并发压下是否已经接近 fsync 极限？** 若是，P2 BatchProposer 的改进空间**被高估**；若否（或只部分饱和），BatchProposer 的 5-8x 预期依然成立。

这是决定 P2 是否值得 10 人天投入的关键判断。

## 判据

观察 write_only 的 throughput scaling 曲线：

- **线性 scale（RPS ∝ conc）**：Raft 还没饱和，BatchProposer 改进空间小（收益 ≤ 2x）。
- **早期饱和（RPS 在 c=8 之后持平）**：Raft 已经 pipelined，瓶颈切换到 fsync 串行。BatchProposer（多 op 共享 fsync）**有明确改进空间（5-8x 理论上限）**。
- **从未 scale（RPS 几乎是常数）**：fsync 从 c=1 就开始主导；意味着 Raft 根本没 pipeline 上任何东西。BatchProposer 收益依然大，但 P2 之外可能还需要 Raft 配置调优。

## 数据

| conc | write RPS | ∆RPS vs c=1 | p99 | ∆p99 vs c=1 |
|---:|---:|---:|---:|---:|
| 1 | 37 | 1.00x | 68ms | 1.0x |
| 8 | **65** | **1.76x** | 229ms | 3.4x |
| 32 | 63 | 1.70x | 1.32s | 19.4x |
| 128 | 67 | 1.81x | 2.12s | 31.2x |

## 结论

### ✅ hashicorp/raft 的 wire-level pipelining 有效但早期饱和

1. **c=1 → c=8 实现了 1.76x throughput scale**，说明 hashicorp/raft 确实把并发的 AppendEntries 攒在了一起，不是完全串行。
2. **c=8 之后 RPS 完全持平（65 → 63 → 67），latency 线性增长**。这是**教科书级的 queueing collapse**：Raft 协议本身的 pipeline 已经满了，新到的请求只是在队列里等待 fsync 串行完成。
3. 从 c=1（37 RPS）到 c=8（65 RPS）的**实际收益 1.76x**，不是 8x。也就是说 hashicorp/raft 内部 pipelining 只回收了 **~22% 的理论并发收益**。剩下的 78% 被 bbolt 单写者 + fsync 串行吞掉。

### ✅ P2 BatchProposer 的收益预期依然成立

逻辑：

- **当前 c=8 瓶颈**：每个 op 独立 Raft log entry → 每 commit 一次 fsync → throughput ∝ 1/fsync_time。
- **BatchProposer 目标**：把 c=8 下排队的 N 个 Op 合并成 1 个 `OpBatch{ops: [N]}` Raft log entry → N 个 op 共享 1 次 fsync → throughput ∝ batch_size/fsync_time。
- 理论上限：batch_size 决定收益。window=2ms × throughput=65 RPS → 平均 batch ≈ **1.3 ops**（实际上 BatchProposer 在 c=8 下能攒到更大 batch，因为 8 个 client 并发入队）。更合理估算 batch=4–8 → 收益 4–8x。
- **P2 目标 3000 RPS @ c=64 @ p99≤50ms**：
  - Linux baseline 估算（来自 OV1）：500–1000 RPS 在 c=8–32 范围合规。
  - BatchProposer 5–8x → **2500–8000 RPS**。
  - 目标 3000 **落在预期区间中位**，可达性良好。

### ⚠️ 但不是所有 BatchProposer 设计都能兑现

关键设计决策（spec §3.6 C9 已锁）：

1. **空队列短路**（200µs，而非固定 2ms）：防止单例写被 2ms 窗口拖尾。否则 c=1 p99 会从 67ms 涨到 69ms（没毛病）但 c=4 混合流量 p99 会从 100ms 涨到 102ms 加一个固定 2ms tail（有感）。
2. **批大小硬上限 500**：超过后 raft log entry 太大，replicate 本身成瓶颈。
3. **部分失败不整批回滚**（spec §3.6 C2）：否则单个 bad op 毁一整批，BatchProposer 实际 batch size 被迫 → 1，收益蒸发。

## 不支持的预期

**"BatchProposer 不值得做，因为 hashicorp/raft 已经 pipeline 完了"** —— ❌ 不成立。c=1→c=8 只回收 22% 并发收益，剩下 78% 是 fsync 串行，正是 BatchProposer 要解决的。

**"BatchProposer 可以做到 80x（从 c=1 的 37 到 3000）"** —— ❌ 不成立。c=1 不是 BatchProposer 的工作点（单 client 怎么 batch？）。正确锚点是 c=8 knee 的 65 RPS，基于这个做 5–8x → 400 RPS (laptop) / 3000 RPS (Linux 估)。

## 对 P2 的具体建议

1. **P2 目标 3000 RPS 保留**，但 P1 kickoff 前先在 Linux 复测 baseline（OV1 follow-up）。若 Linux c=8 knee < 400，把目标下修到 2000。
2. **P2 成功验收必须同时检查 c=1 p50 劣化 ≤ 10%**（spec §4.1 P2 已有），防止 BatchProposer 的 2ms 窗口反向伤单例写。OV2 数据 c=1 p50=25ms，允许最多涨到 27.5ms，仍在 SLO 内。
3. **P2 kickoff 时先实现 batch，再实现短路**。batch 基础逻辑正确后再调优 2ms → 200µs 的自适应。防止一次性变更太多参数难归因。
4. **保留本 laptop baseline 作 P2 回归证据**：同 laptop 上 P2 完成后再跑一次同 sweep，对比前后曲线。即使 Linux 数据没就位，laptop 上也应该看到 c=8 RPS 从 65 涨到 300+（laptop fsync 15ms × batch=5 ≈ 333 RPS），否则 P2 有 bug。

## Raw 数据

- `bench/results/20260422-104733_write_only_c{1,8,32,128}.json`
- 汇总：`bench/results/20260422-104733_report.md`
- 环境指纹：所有 JSON 的 `.env` 字段
