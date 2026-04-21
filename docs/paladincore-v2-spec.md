# PaladinCore v2 — 性能竞争力重构规格

> 一份专门回答 "如何让 PaladinCore 的压测数据接近或超过业界开源实现、同时保持轻量级" 的规格文档。
>
> 与 `production-refactoring.md` 互补：那份是全量生产化蓝图（18 人周，八大主题），**本文是其中 "benchmark-competitive + reference-implementation-grade" 子集的深度论证** (~66 人天，8 个阶段)。
>
> 目标读者：执行人。你会被告知 **做什么** 与 **为什么这样做**；实现细节由你决定。
>
> 风格约束：每条陈述必须可验证；每个决策必须有具体论据而非 "业界最佳实践"；每个数字必须可复现。

---

## 零、项目气质（读本规格之前必读）

> **PaladinCore v2 是一个 rigor-over-marketing 项目。**
>
> 我们选 Jepsen 等价验证，不选闪亮 dashboard；选叙事深度，不选功能数量；选把"为什么不做 X"写清楚，不选假装做了 X。
>
> **目标不是打动人，是让一个审视者信服。**
>
> 这条立场是所有设计决策的隐含裁决标准。当两条技术路径同等可行时，**更严谨、更可审计、更诚实的那条胜出**，即使它的 headline 数字不那么漂亮。例如：拒绝 multi-raft 分片（不靠架构复杂度刷数字）、拒绝公开实时 dashboard（不把营销当产品）、接受 Jepsen 级混沌测试（正确性必须可审）、接受 LOC 从 5000 放宽到 6000 仅为了容纳 gRPC（写路径真实场景必需）——这些都是这条立场的直接推论。
>
> 如果读完本规格你觉得某个决策"缺乏野心"，请回到这一章。本项目的野心不在数字上，在**可被世界级审视者信服**这件事上。

---

## 一、问题定义

### 1.1 核心问题陈述

**为谁**：打算以 PaladinCore 作为简历/技术分享/开源作品基石的工程师，以及评估其能否作为"配置中心场景下轻量级替代 etcd"的潜在采用者。

**什么问题**：当前 PaladinCore 的压测数字（写 RPS ~55 @ p99 523ms，读 RPS ~94k @ p99 550µs，3 节点 localhost）既不能支撑"我的作品够严肃"的主张，也不能和 etcd 等同代开源项目做可信对比——**不是因为架构不行，而是因为测量、优化、回归防护三块结构性能力没有闭环。**

**为什么现在**：基础设施地基（structured logging、Prometheus metrics、goleak、typed sentinel errors）已落地；下一步是从"语法正确"跨到"性能可信"。在这个窗口期定义好测量纪律与优化边界，后续 sprint 可按预期推进；错过这个窗口，后面每次优化都会各说各话。

**成功的定义**（五条同时满足即 v2.0 达标）：

1. 存在 `docs/benchmarking-v1.md`，任何人在一台常见 Linux 服务器上 30 分钟内能复现规格中所有 headline 数字的 ±10%。
2. 同环境同 workload 下，写 RPS ≥ etcd 的 50%，读 RPS ≥ etcd 的 100%（workload 形状：64B–4KB value、10k 级 key、95/5 读写比、conc=64、3 节点 fsync=on）。
3. 非测试代码 ≤ **6000 LOC**，在 CI 强制（原 5000 上限因 gRPC Watch 新增 ~1000 行预算上调；见决策 8）。
4. bench CI 闭环运行至少 60 天未漏报任何 >5% 退化。
5. 每个性能优化 PR 都附"延迟分解表（前 vs 后）"，证明命中预期分解项。
6. **Jepsen 等价混沌测试（porcupine linearizability + toxiproxy TCP 分区注入）** 在 CI 中连续 30 天零 linearizability 违反。
7. **5 篇深度技术文章**（workload shape / Raft 选择 / 延迟分解 / group commit / fsync-dominated 写路径）发布并配可独立运行 demo。

**"接近或超过 etcd" 的精确含义**：不是所有维度上超过，而是在 **配置中心 workload + LOC 归一化** 两个维度上显著胜出。等效表述："以 etcd 1/60 的代码量，在配置中心工作负载上拿到 etcd 50%–150% 的性能。"

### 1.2 设计北极星

> **配置中心工作负载下的 throughput-per-line-of-code 天花板。**

当两条设计路径发生冲突时（例如某优化提速 30% 但加 500 行代码），以下面单一标准裁决：**它是否提升了在 64B–4KB/95-5/conc=64 这一标准工作负载下的 RPS÷LOC 比值？** 不是，就不做。

### 1.3 非目标（硬约束）

为防止作用域蔓延，明确**不做**：

- **不做 MVCC**。配置不需要时间旅行；旧版本无人读。代价：`?rev=N` 历史查询放弃。
- **不做 Lease**。配置不过期；心跳续约是服务发现场景的需求，与配置中心形状不符。
- **不做多 DC 复制 / 异步 replica**。单 Raft group。跨 DC 请部署多独立集群，应用层聚合。
- **不做 RBAC / 细粒度权限**。靠网络隔离 + 静态 token；不建 policy 引擎。
- **不做插件 / 脚本 / 扩展点**。每条用户能用的能力都在核心代码里、可审、可压测。
- **不做大 value（> 256KB）支持**。value 超限直接返 413；配置中心就不该存二进制。
- **不做 multi-raft / sharding**（见第九章 Limitations 的详细论证）。写吞吐 ~3 万/s 级封顶是 rigor-over-throughput 立场的必然代价。
- **不做公开连续 benchmark dashboard**（见第九章）。benchmark baseline JSON 留在 repo 内，不建公开 Pages；rigor 向内不向外。
- **不以 "beat etcd on 所有 workload" 为目标**。通用 KV 上 etcd 胜是预期结果；不必辩护。

**被移出 Non-Goal 的（v2 现纳入）**：

- **gRPC/HTTP2 作为 Watch 一等 API**——原 Non-Goal 在本次 CEO review 中被推翻。理由：HTTP/1.1 长轮询在 10k+ watcher 场景下是结构性瓶颈，硬撑"HTTP/1.1 only"违背 "headline 数字诚实" 原则。v2 窗口内 gRPC Watch 必须落地；HTTP/1.1 作为兼容路径保留。见决策 8（新）与 P8 阶段。

---

## 二、核心设计决策

**决策 1：把测量纪律作为架构组件，而非工程习惯。**
- 选择：bench CI 闭环 + baseline 文件 + 延迟分解表 = 一等公民。每个性能 PR 必须附前后分解对比；CI 自动对比 baseline，>5% 退化 fail；LOC 硬上限也在 CI 中强制。
- 替代方案：靠 code review 维持纪律 / 定期手动压测 / 上线后观测。
- 理由：阶段三的失败考古显示，纯靠纪律的性能优化 100% 的团队最终会 regress。把纪律变成机器检查是唯一结构性解。
- 代价：前期 5 人天投入无性能收益；PR 合并变慢 10 分钟（quick-bench 运行时间）。
- 逆转条件：如果 quick-bench 在 CI 里运行时间 > 20 分钟且无法缩短，改为每日 cron 而非 per-PR。但基线对比仍保留。

**决策 2：Workload 特化 > 通用性。**
- 选择：在设计里显式利用"读写比 ≥ 1000:1、值 ≤ 4KB 常态、key 集合近静态"三条工作负载性质，允许做出 etcd 无法做的简化。
- 替代方案：维持通用 KV 语义，靠纯优化追 etcd。
- 理由：Top 10% 的团队走通用路径最多到 etcd 70% 性能 + 40% 代码；Top 1% 走特化路径能以 <10% 代码做到 100%+ 性能（在特化 workload 上）。这是 v2 能成立的唯一战场。
- 代价：明确声明 "不是通用 KV"；失去一批潜在 etcd 用户。
- 逆转条件：如果数据显示实际用户 workload 远离 95/5 读写比，重新评估。

**决策 3：headline 数字必须 fsync=on + 3 副本 + 跨网络。**
- 选择：公开材料中的主数字必须满足上述三条；任何 fsync=off / 单节点数字必须打 "SYNTHETIC" 标签。
- 替代方案：跟随某些项目的做法，给出"理论峰值"数字。
- 理由：阶段三的 "Fsync Denial" 失败模式。一旦允许宽松配置的数字作为主数字，整个项目的可信度透支。
- 代价：headline 数字数值上没有"宽松配置"版本好看。
- 逆转条件：无。这是长期可信度的基线。

**决策 4：一次 wire 跃迁（Raft 内部 Op JSON → protobuf），此后严格前后兼容。**
- 选择：v2 窗口完成 Raft 内部 Op 的 wire 换成 protobuf；HTTP API 保持 JSON 不动。
- 替代方案：全量 protobuf / 一直 JSON / 切 MessagePack。
- 理由：JSON 在 hot path 上的 CPU 开销实测 1.5–3× 收益。全量 protobuf 破坏 HTTP API 的可调试性。MessagePack 无 schema，前后兼容保证弱。protobuf 是"schema + 性能 + 工具链"的最优组合。
- 代价：+ ~300 行生成代码；团队需要 `.proto` 维护纪律。
- 逆转条件：如 pprof 显示 protobuf 已不在 top 5 热点，未来可以不演进；但不回滚。

**决策 5：BatchProposer 是写路径的唯一战略优化。**
- 选择：在 `raft.Node.Apply` 之前加一层 `BatchProposer`，1–5ms 自适应窗口合并提案，通过 `raft.ApplyBatch` 提交。
- 替代方案：fork hashicorp/raft / 替换为 dragonboat / 异步 Apply。
- 理由：fork Raft 库是负担永久代价换边际收益。dragonboat 性能更优但生态小、读者成本高。异步 Apply 与 FSM.Apply 同步语义冲突。group commit 是 ROI 最高且复杂度最低的选项。
- 代价：低并发下 + 1–5ms 延迟地板（自适应窗口缓解，单请求场景窗口塌为 0）。
- 逆转条件：测出 batching 非瓶颈 / 业务拒绝 + 1ms 地板。

**决策 6：WatchCache 发布侧改为 channel-based signal，不做架构重写。**
- 选择：保留 ring buffer 结构，把 `sync.Cond.Broadcast` 换成 atomic generation counter + channel signal；订阅者无锁读。
- 替代方案：pub/sub 组件 / goroutine-per-watcher 推送。
- 理由：当前架构语义正确，仅锁方式有性能问题。组件级替换对 1k 以下 watcher 数收益有限且违反 LOC 预算。
- 代价：在 10k+ 并发 watcher 场景下仍有封顶（约 5 万 event/s）；超此量级需 v2.1 引入 HTTP/2 推送。
- 逆转条件：实测 10k+ watcher 成为主流用户场景。

**决策 7：保留 hashicorp/raft 和 bbolt 作为基础设施。**
- 选择：不替换。
- 替代方案：dragonboat、自研 Raft、badger、rocksdb。
- 理由：两者合计为 PaladinCore 提供了 ~20k 行经生产验证的代码。替换任何一个都需要 2–3 个月 + 引入长期维护债。v2 不做这种战略级替换。
- 代价：性能天花板受这两个库影响（bbolt 单写、hashicorp/raft pipelining 粒度有限）。
- 逆转条件：明确测到某个库是 critical path 且优化其参数空间后仍不够。

**决策 8（原）：HTTP/1.1 + JSON 作为 v2 的主 API。 → 已被决策 8（新）推翻**。

**决策 8（新）：Watch 路径双传输——gRPC 为默认，HTTP/1.1 长轮询为兼容回退。**
- 选择：新增 gRPC streaming Watch 为默认路径（SDK 默认、新 client 默认）；HTTP/1.1 长轮询保留作为老客户端兼容。Put/Get/Delete 仍走 HTTP/1.1 + JSON（未变）。
- 替代方案：
  1. 仅 HTTP/1.1（被本决策推翻）；
  2. 纯 net/http HTTP/2 server push（无 proto/grpc 生态，fanout 上限 ~10k，非 50k）；
  3. 全量 gRPC 含 Put/Get（破坏 JSON API 的可调试性）。
- 理由：HTTP/1.1 长轮询在 10k 并发 watcher 下 fanout 封顶 ~1k 实际并发——**这是相对 etcd 的结构性劣势，不可辩护**。gRPC bidi stream 让 fanout 天花板升到 ~5 万。纯 HTTP/2 省依赖但牺牲 multiplex 粒度，且客户端库生态不如 gRPC 成熟。
- 代价：+ ~1000 行代码（proto 定义 + streaming server + SDK 双路径）；LOC 预算 5000 → 6000；SDK 维护复杂度 +30%。
- 逆转条件：若 10k watcher 场景实测显示 HTTP/2 server push 性能同量级且代码更省，可在 v3 评估替代 gRPC。

**决策 9：gRPC Watch 的 slow-consumer 策略——踢 stream 回退 fullPull。**
- 选择：服务端给每个 gRPC stream 维护 1024 事件的 ring 缓冲；超过此即关闭 stream 并返回 `RESOURCE_EXHAUSTED`。SDK 从最后已知 revision 触发 fullPull + 重新建立 stream。
- 替代方案：
  1. 无限队列 + backpressure 阻塞——会让一个慢客户端 hang 住整条 WatchCache 广播路径（结构性危险）；
  2. 悄悄丢事件 + 下一个事件带 "previous-dropped" 标记——客户端 UX 不可预测。
- 理由：与项目 "rigor over cleverness" 立场一致。踢流是可预测、可审计、可测试的行为；它让慢客户端问题显式暴露而非静默恶化。
- 代价：瞬时网络抖动可能触发一次 fullPull + 重连（几百 ms 额外延迟）。
- 逆转条件：若实测 90% 以上的踢流由正常客户端行为触发（非真慢客户端），调大 ring 或改为复合阈值。

**决策 10：混沌/正确性验证栈——porcupine + toxiproxy（纯 Go 生态）。**
- 选择：用 `github.com/anishathalye/porcupine` 做 linearizability 离线校验（MIT 6.824 教学标准），搭 `toxiproxy` 做 TCP 分区/延迟注入。发布前跑完整 chaos suite，CI 每 PR 跑缩版（partition × 2 + node-kill × 1）。
- 替代方案：
  1. 引入真正的 Jepsen（Clojure）——名字营销价值高但引入 JVM/Clojure 依赖；违反 "Go-native lightweight"，新贡献者门槛 +50%；
  2. 自研 linearizability checker + partition 注入——重造轮子，不如用现成成熟方案。
- 理由：porcupine 是 MIT 教学级参考实现，在学术与工业界都被接受；toxiproxy 是 Shopify 开源的 TCP 代理层，主动制造网络异常；两者组合在 Go 生态内提供 80% 的 Jepsen 能力，剩下 20%（如时钟控制）用 `monotime` fake clock 补足。
- 代价：15 人天建设成本；首次跑完整 suite 可能发现现有实现的潜在 bug（进度风险）。
- 逆转条件：若发现 porcupine 在 PaladinCore workload 规模下过慢（>1 小时 / PR），改为每日 cron 而非 per-PR；但发布前必跑。

**决策 11：WatchCache gap 阈值——1024 事件 ring buffer，事件数粒度。**
- 选择：每个 namespace 维护 1024 事件的 ring buffer（约 0.5–2 分钟热事件窗口，取决于变更率）。SDK 重连携带上次 revision；若该 revision < ring 最旧 revision，服务端返回 `ErrGapTooLarge`，SDK 自动走 fullPull 再恢复 stream。
- 替代方案：
  1. 时间阈值（5 分钟）——需要给每条 ring entry 打时间戳，实现更复杂；
  2. 双阈值 `max(1024 事件, 5 分钟)`——测试组合爆炸；
  3. 无 ring，每次重连强制 fullPull——简单但长连保持率差。
- 理由：事件数粒度与 ring 天然对齐，实现和测试最简；1024 在典型 config-center 场景覆盖 >60s 重连窗口，足以吸收一次 full GC / 短暂网络抖动。
- 代价：高变更率场景下 ring 窗口可能收窄到 <20s；仍然小于 SDK 默认重连超时，可接受。
- 逆转条件：实测 ring 耗尽强制 fullPull 的比例 > 5%，扩大 ring 或改为双阈值。

---

## 三、技术架构

### 3.1 系统全景

```
          Client                                    Chaos Harness
   ┌───────────────────┐                     ┌─────────────────────┐
   │ HTTP/1.1 + JSON   │                     │ toxiproxy (TCP      │
   │ (Put/Get/Delete)  │                     │  partition inject)  │
   │ gRPC streaming    │                     │ porcupine (lin-     │
   │ (Watch, default)  │                     │  earizability chk)  │
   │ HTTP long-poll    │                     └─────────────────────┘
   │ (Watch, legacy)   │                               │ (test env)
   └───────────────────┘                               │
              │                                        ▼
              ▼                             ┌─────────────────────┐
   ┌─────────────────────┐                  │ CI: per-PR quick +  │
   │ PaladinServer (any) │ ◀───proxy──────► │     pre-release     │
   └─────────────────────┘                  │     full chaos suite│
         │ reads               writes       └─────────────────────┘
         │ (local)             │
         ▼                     ▼
   ┌─────────┐       ┌─────────────────────┐
   │ bbolt   │       │ BatchProposer       │   1–5ms 自适应窗口
   │  View   │       └─────────────────────┘
   └─────────┘                 │ raft.ApplyBatch([]Op)
                               ▼
                   ┌─────────────────────┐
                   │ hashicorp/raft      │   pipelined append
                   │  (Leader)           │   + group fsync
                   └─────────────────────┘
                          │ replicate
              ┌───────────┼───────────┐
              ▼           ▼           ▼
         [Follower]  [Follower]  [Follower]
              │           │           │
              ▼           ▼           ▼
         FSM.Apply   FSM.Apply   FSM.Apply
              │
              ▼
         bbolt.Tx
              │
              ▼
   ┌─────────────────────────────────────┐
   │ WatchCache (ring buf 1024/ns)       │
   │   ├─▶ gRPC stream fanout (default)  │──▶ SDK gRPC client
   │   └─▶ HTTP long-poll (legacy)       │──▶ SDK HTTP client
   └─────────────────────────────────────┘
```

核心原则：**单路径（写）+ 多路径（读）+ 双传输推播路径（watch）+ 离线混沌验证回路** 四条独立优化/验证曲线。混沌路径只在测试/预发环境启用，生产零额外开销。

### 3.2 核心模块规格

#### `BatchProposer`（新增）
- **职责**：把并发到达的单条 Raft Op 合并成批，减少 Raft log append 和 fsync 次数。
- **输入 → 输出**：`Apply(op Op) → (*OpResult, error)` 外观不变；内部合并。
- **核心逻辑（伪码）**：
  ```
  ch := 单入口 channel<Op+resultFuture>
  goroutine loop:
    batch := []
    deadline := 5ms from first pending op
    until deadline or len(batch) >= 500:
      select recv from ch → batch.append
      select deadline.fire → break
    raft.ApplyBatch(batch)  // one pipelined round-trip
    distribute results to each future
  ```
- **失败处理**：单条 Op 失败只 fail 它自己的 future，不污染整批；批内 Raft commit 全失败才整批回滚。
- **性能约束**：单批 ≤ 500 Op；单条 Op 排队 ≤ 5ms；内部 channel 非阻塞入队失败直接 503。

#### `Node.Apply` / `FSM.Apply`（既有，变更：接受批）
- **职责**：把单批 Op 串行 apply 到 bbolt，确保事务原子性；更新 store_revision 与 WatchCache。
- **关键变更**：`FSM.Apply` 循环处理 log.Data 中的 `[]Op`，单个 bbolt Tx 内全部完成。
- **失败处理**：任一 Op 语义失败（如 delete 不存在的 key）只标记该 Op 的 Err，其他 Op 仍提交。
- **性能约束**：单批 Apply 总耗时 ≤ 10ms（bbolt fsync 典型 < 1ms × 一次）。

#### `WatchCache`（既有，变更：发布侧无锁）
- **职责**：维护最近 4096 条变更事件的环形缓冲；多订阅者独立游标读。
- **关键变更**：
  - 发布：atomic uint64 generation counter；每次 append 递增并发送 signal 到一个 struct{} channel（带 buffer=1）。
  - 订阅：`WaitForEvents(afterRev, prefix, timeout)` 读 generation；如无新事件则 select channel + timeout。
- **失败处理**：ring 溢出 = 最旧事件被覆盖；订阅者发现 gap 后返回错误，SDK 触发 fullPull。
- **性能约束**：append 延迟 < 1µs（排除 bbolt 提交）；10k waiter 唤醒总延迟 < 10ms。

#### `Server`（既有，变更：零分配热路径）
- **职责**：HTTP 路由 + 鉴权（v2 无鉴权）+ 延迟统计。
- **关键变更**：
  - 路径 parse：`strings.Cut` 替换 `strings.Split`，零 alloc。
  - `ConfigResponse` buffer：`sync.Pool` 复用（需验证无数据残留）。
  - hot path 禁用 `fmt.Sprintf`，改 `strconv.AppendUint` + `bytes.Buffer`。
- **失败处理**：所有 4xx/5xx 走 `httpError` 单入口，带 `Request-ID`。
- **性能约束**：单请求解析 < 1µs；JSON 序列化 < 10µs per 1KB response。

#### `BoltStore` / `WatchableStore`（既有，轻微调整）
- **职责**：持久化 KV + 发事件。
- **关键变更**：
  - `Put` 接受 `[]Op` 批次（供 FSM.Apply 单 Tx 调用）。
  - `List` 的结果复用一个 `[]*Entry` pool。
- **性能约束**：单批 500 个 Put 总耗时 ≤ 5ms @ NVMe。

#### `WatchStream` (gRPC，新增，P8 阶段)
- **职责**：给 SDK 提供 streaming Watch，代替 HTTP/1.1 长轮询作为默认传输。
- **proto 定义**：
  ```protobuf
  service WatchService {
    rpc Watch(stream WatchRequest) returns (stream WatchEvent);
  }
  message WatchRequest {
    string tenant    = 1;
    string namespace = 2;
    string prefix    = 3;   // 可选
    uint64 after_rev = 4;   // 客户端已知的最后 revision
  }
  message WatchEvent {
    uint64 revision = 1;
    string key      = 2;
    bytes  value    = 3;    // DELETE 时为空
    EventType type  = 4;    // PUT / DELETE
    bool    gap_fullpull_required = 5;  // 服务端通知客户端触发 fullPull
  }
  ```
- **核心逻辑（伪码）**：
  ```
  上游 WatchCache.Subscribe(tenant, ns, prefix, after_rev)
  建立 per-stream 1024 事件 ring 缓冲区
  loop:
    select {
      case ev := <-cache.notify:
        if buffer.len() >= 1024:
          stream.SendAndClose(RESOURCE_EXHAUSTED)
          metrics.inc("grpc_slow_consumer_kicks_total")
          return
        buffer.push(ev)
        stream.Send(ev)
      case <-stream.Context().Done():
        return
    }
  if after_rev < cache.oldest_revision:
    return ErrGapTooLarge  // 客户端需 fullPull
  ```
- **失败处理**：
  - Slow consumer（decision 9）：关闭 stream + RESOURCE_EXHAUSTED，SDK fullPull+重连。
  - Gap too large（decision 11）：stream 建立时若 `after_rev < cache.oldest` 直接返 `ErrGapTooLarge`。
  - Server 关闭：stream 收到 `UNAVAILABLE`，SDK 透明重连。
- **性能约束**：单 stream 吞吐 ≥ 5k event/s @ 100 watcher；server 总 fanout 容量 ≥ 5 万 event/s @ 10k stream。

#### `ChaosHarness` (新增，P6 阶段)
- **职责**：在 CI/预发环境用 `toxiproxy` 对 Raft TCP 端口注入网络异常，用 `porcupine` 离线校验操作序列的 linearizability。
- **核心组件**：
  1. **partition injector**：基于 toxiproxy，动态对 `raft-leader ⇄ raft-follower` TCP 连接注入延迟/断开/包丢弃。
  2. **scenario runner**：标准混沌 scenarios 目录（`chaos/scenarios/*.yaml`），每个描述 partition 时序 + 并发 workload（put/get/delete）。
  3. **history recorder**：workload 在整个 scenario 期间每一次 op 的 `(invoke_time, op, response_time, response)` 记录到文件。
  4. **linearizability checker**：scenario 跑完离线运行 porcupine 校验 history；若违反线性化，scenario 标记为红。
- **验收**：
  - per-PR quick suite: 2 个 partition scenario + 1 个 node-kill scenario，15 分钟内完成，必须 100% pass。
  - pre-release full suite：10+ scenarios（含 clock skew、slow disk、majority loss）、4 小时内完成，必须 100% pass。
- **性能约束**：quick suite ≤ 15 分钟；full suite ≤ 4 小时；porcupine 单 scenario 校验 ≤ 60 秒。

### 3.3 数据模型

沿用当前模型。**关键约束**（强制）：

| 字段 | 类型 | 约束 |
|---|---|---|
| key | string | ≤ 256 字节；UTF-8；不含 `\x00` |
| value | []byte | ≤ 256 KB（超限 413） |
| revision | uint64 | 集群单调递增，由 Raft commit 顺序决定 |
| tenant/namespace | string | ≤ 64 字节；仅 `[a-z0-9-]` |

**生命周期**：创建 → 任意次更新 → 删除（tombstone + 立即可回收，无 MVCC 历史）。

**内部 Raft Op wire（proto 定义）**：

```protobuf
message Op {
  enum Type { PUT = 0; DELETE = 1; }
  Type   type  = 1;
  string key   = 2;
  bytes  value = 3;  // only for PUT
}

message OpBatch {
  repeated Op ops = 1;
}
```

外部 HTTP/JSON API 不动。

### 3.4 API 契约

保持当前 HTTP API，**新增**：

- `GET /api/v1/config/...?consistent=true` — 走 ReadIndex（v2.P3 实现）；延迟约 2× stale read。
- `PUT /api/v1/config/...` 新增可选 header `If-Match: <rev>`——实现 CAS；不匹配返 412。
- `PUT` 响应 header 恒保留 `X-Paladin-Revision`；客户端重试必须携带 `Request-ID`（v2 不做去重，但约定好，v2.1 再做）。
- **新增 gRPC 端点**：`WatchService.Watch(stream WatchRequest) → stream WatchEvent`（监听 `:<grpc_port>`，默认与 HTTP 端口 `+1000`；proto 定义见 `WatchStream` 模块）。
  - 正常路径：客户端发起 stream 后持续收到 event；断流时带 gRPC status code 指示原因。
  - 错误路径：
    - `RESOURCE_EXHAUSTED` — slow consumer，客户端 fullPull + 重连（决策 9）。
    - `FAILED_PRECONDITION` with detail `ErrGapTooLarge` — 起始 revision 已滚出 ring，客户端 fullPull 再订阅（决策 11）。
    - `UNAVAILABLE` — server 关闭或网络中断，客户端透明重试。
  - 幂等性：同 revision + 同 key 的 event 在重连后可能重复投递；SDK 层基于 `revision` 去重。

错误码对齐 `production-refactoring.md` 附录 B，gRPC 侧映射到标准 status code。

### 3.5 构建与代码生成（P1+ 引入 protobuf 后）

- **proto 源文件位置**：`proto/` 顶层目录；每个服务一个 `.proto`（如 `proto/raft_op.proto`, `proto/watch.proto`）。
- **生成代码**：`*.pb.go` 和 `*_grpc.pb.go` **committed 进 repo**（不走运行时 `go generate`），保证纯 `go build` 可复现。
- **make 目标**：`make proto` 调用 `protoc` + `protoc-gen-go@v1.34` + `protoc-gen-go-grpc@v1.5`（版本钉死在 `tools/tools.go` + `go.mod`）。CI 有 check 确保 `make proto` 后 git tree 干净（否则意味着开发者改了 `.proto` 但没 regen）。
- **linter**：`buf lint` 在 PR CI 跑；禁止破坏性 proto 变更（字段重命名、tag 修改）触发失败。
- **LOC 预算豁免**：`*.pb.go` / `*_grpc.pb.go` **不计入 6000 行非测试代码硬上限**（自动生成，不是人写的）。`make loc-budget` 脚本排除这些。

### 3.6 Eng-Review 锁定契约（2026-04-21 review 沉淀）

以下契约是 eng review 产出，实现时必须照做；违反需新开一次 review 翻案。

#### 契约 C1：Raft 日志格式迁移（issue 1）

- **格式字节前缀**：所有 `log.Data` 第 1 字节为 format tag：`0x00` = legacy JSON，`0x01` = protobuf。
- **写路径**：v2 起所有新写入强制 `0x01 | protoBytes`。
- **读路径（FSM.Apply）**：`switch data[0]` 分支；`0x00` 走 `json.Unmarshal`（deprecation 窗口内兼容），`0x01` 走 `proto.Unmarshal`，其他 byte 返 error 并触发 metric `paladin_raft_unknown_log_format_total`。
- **Deprecation 窗口**：v2.0–v2.2 双解码；v2.3 起写路径拒绝生成 `0x00`（不影响读）；v2.6 起读路径也拒绝 `0x00`，拒绝启动如 raft-log.db 仍有 `0x00` 条目。
- **强制 regression test**：`raft/migration_test.go` 用 fixture `testdata/pre_v2_raft_log.bin` 回放，断言 byte-for-byte 正确。

#### 契约 C2：批 FSM.Apply 部分提交语义（issue 2）

- **framework 级错误**（proto unmarshal 失败、bucket 不存在）→ 整批 Tx 回滚，返回 error。
- **业务级错误**（delete 不存在的 key、unknown op type）→ 该 Op 的 `OpResult.Err` 置位，**Tx 仍 commit**，其他 Op 正常写入。
- 实现注意：不能在 `db.Update(func(tx) error)` 里 `return err` — 那会回滚整 Tx。业务错误存在 slice 里，Update 返回 nil。
- **测试**：`TestFSMApplyBatch_PartialFailure`、`TestFSMApplyBatch_FrameworkFailureRollback`。

#### 契约 C3：WatchCache 懒分配 + 空闲 GC（issue 3）

- **分配时机**：首个订阅者到达时创建该 namespace 的 ring buffer。未被订阅的 namespace 不分配。
- **容量**：每 namespace ring 1024 条 Event（默认，可 config）。
- **空闲回收**：后台 GC goroutine 每分钟扫描；若 namespace 满足（`len(subscribers) == 0`）AND（`time.Since(lastActivity) > 10min`），drop cache。下次订阅者触发重建（会 `FAILED_PRECONDITION` → SDK fullPull）。
- **硬上限**：全局最多 2000 个活 cache；超限时先 evict 最久未活动的；触发 metric `paladin_watchcache_evicted_total`。

#### 契约 C4：Chaos Harness 拓扑构建器（issue 4）

- P6 第 1–2 天交付 `chaos/harness/topology.go`：输入 `Topology{Nodes: 3, ProxyPort: 18000}`，启动 `toxiproxy-server`，按 `N×(N-1)` 对等规则创建 proxy 对，再启 N 个 paladin 节点，peer config 指向 proxy 侧地址。
- scenario 文件通过 `topology.API()` 调 toxiproxy 管理 HTTP API 注入 latency/partition。
- **架构图补丁**：ChaosHarness 子系统需显式画 `toxiproxy-server`→`proxy pair (A→B, B→A)`→`paladin node` 三层。

#### 契约 C5：SDK 双传输接口（issue 5）

```go
// sdk/watch_transport.go
type WatchTransport interface {
    // Subscribe blocks until ctx is done or transport fatally errors.
    // Emits events on returned chan; closes chan on termination.
    Subscribe(ctx context.Context, tenant, ns string, afterRev uint64) (<-chan Event, error)
    Close() error
}
```

- `HTTPWatchTransport`：现 `watchLoop` 重构包。
- `GRPCWatchTransport`：新实现，相同接口。
- `Client` 持有一个 `WatchTransport`，由 config 选择；`applyEvents` / 缓存逻辑不感知传输层。
- **接口位于 SDK 内部**，不 export 给用户；用户只看 `NewClient(Config{Transport: sdk.TransportGRPC})`。

#### 契约 C6：gRPC 端口与 DoS 上限（issue 6 + P3）

- **CLI 参数**：新增 `--grpc-addr`，默认派生 = HTTP 端口 + 1000；两者同时给以 `--grpc-addr` 为准；派生值与 HTTP/Raft 端口冲突则启动失败。
- **服务端上限**：
  - 单 IP 最多 1000 个活跃 stream（超限返 `RESOURCE_EXHAUSTED` "per-ip limit"）；
  - 全 server 最多 50000 个活跃 stream（超限返 `RESOURCE_EXHAUSTED` "server limit"）；
  - 均暴露为 metric：`paladin_grpc_limit_rejections_total{reason="per_ip|server"}`。

#### 契约 C7：`BoltStore` 批 API（Q1）

- 新增 `PutBatch(ops []PutOp) ([]*PutResult, error)` 和 `DeleteBatch(keys []string) ([]*Entry, error)`——单 `db.Update` 内循环，单次 fsync。
- 事件收集到 slice，最后一次性 `WatchCache.AppendBatch(events)` + 单次 `Broadcast`。
- 既有 `Put/Delete` 保留（单 op 路径走 BatchProposer 的批可能只含 1 个 op，但 API 不变）。

#### 契约 C8：Metrics Registry 约定（Q3）

- **服务端代码**（`server/`, `raft/`, `store/`, `internal/`）：一律注册到 `internal/metrics.Registry`（包级全局）。
- **SDK / 库代码**（`sdk/`）：per-instance `*prometheus.Registry`，由 top-level 对象 own。
- **违反检测**：golangci 自定义 linter（优先级低，短期靠 PR review）。

#### 契约 C9：BatchProposer 空队列短路（P1）

- enqueue 时若队列长度为 0，window 定时器设置为 `200µs`（而非 2ms）；首个 Op 到达即开始计时。
- enqueue 时若队列长度 > 0，已存在的定时器不修改。
- 目标：单例写场景 p99 增量 < 500µs；爆发写场景仍能攒满 500 op / 2ms 窗口。

#### 契约 C10：Porcupine 史记上限（P2）

- 每 scenario 记录的 history 上限 **1000 operations**（超限按均匀采样）。
- 每 scenario 的 porcupine 校验 wall-clock **60s 硬上限**；超时 → scenario fail 且打点 `paladin_chaos_porcupine_timeout_total`。
- `chaos/runner` quick suite 场景 ≤ 3，full suite（nightly）场景 ≤ 10。

---

## 四、实施计划

### 4.1 分阶段路线图

**P0 — 测量地基（5 人天）**

- 交付：
  - `docs/latency-decomp-v0.md`：写路径 ns 级分解（含 JSON marshal / raft Propose / raft commit / FSM Apply / bbolt Tx / fsync / JSON marshal response 各阶段耗时）。
  - `scripts/bench-vs-etcd.sh`：同一硬件上跑 etcd 3.5.x 同 workload 产出对比表。
  - bench CI quick-suite（10 分钟）+ baseline diff（>5% 退化 fail）。
  - `make loc-budget`（CI 强制非测试代码 ≤ 6000 行，proto 生成代码除外）。
  - **新增（eng-review OV1）**：`docs/p0-raft-ceiling.md`——**未改动现有代码**时，hashicorp/raft + bbolt + fsync=on 在 Linux VM 3 节点集群上的极限写 RPS 测量。作为 P2 KPI 的可达性锚点。
  - **新增（eng-review OV2）**：`docs/p0-raft-pipelining.md`——验证 hashicorp/raft 内置 wire-level batching 在并发写下是否已接近 fsync 极限；若是，P2 BatchProposer 的 speedup 预期下修；若否，P2 仍按计划推进。
- 验收：六项工件存在且在 CI 上 green 跑过一次；对比表含 etcd 和 PaladinCore 各 12 个数据点；P0 结论**必须在 P1 kickoff 前写入 §五 KPI 表**——若实测 ceiling < 20k RPS，调低 KPI，不虚报。
- 依赖：无。
- 风险：etcd 环境搭建卡 1–2 天；缓解：用官方 docker-compose。

**P1 — 低挂果（4 人天）**

- 交付：
  - Raft Op wire 改 protobuf；hot path 去 JSON marshal。
  - **Eng-review OV6**：proto schema 从 day 1 定义为 `message OpBatch { repeated Op ops = 1; }`；P1 写路径临时 wrap 单 op 为 `OpBatch{ops: [op]}`；P2 真正批提交时沿用同 schema。**一次 wire jump，决策 4 兑现。**
  - 实现 C1 的格式字节 + dual-decode 分支。
  - 路径 parse 零 alloc；`context.Background()` 移出 hot path。
  - `sync.Pool` 复用 ConfigResponse buffer（含 goleak-style 检测）。
- 验收（硬指标）：写 RPS @ conc=64 @ p99 ≤ 50ms ≥ 150；读 RPS @ conc=64 ≥ 200k；每项优化附前后延迟分解表；`raft/migration_test.go` 过。
- 依赖：P0。
- 风险：`sync.Pool` 用错泄漏数据；缓解：test 里在每次 Put 后人工断言 buffer 清洁。

**P2 — 写路径重构（10 人天，价值最大）**

- 交付：
  - `BatchProposer` + `FSM.Apply` 接受批次。
  - Raft 参数调优（`MaxAppendEntries`、`SnapshotThreshold` 等），每个值都有 A/B 测试结果支撑。
  - bbolt 单 Tx 批 Put。
- 验收：写 RPS @ conc=64 @ p99 ≤ 50ms ≥ 3000；单请求 p50 @ conc=1 劣化 ≤ 10%；latency decomp 中 raft Propose→commit 从主导退为 < 30%。
- 依赖：P1。
- 风险：batching 窗口参数选错引入尾延迟；缓解：A/B 0/1/2/5ms 四种，选拐点。

**P3 — 读 & Watch 优化（5 人天）**

- 交付：
  - `?consistent=true` ReadIndex 路径。
  - WatchCache 发布侧 atomic + channel signal。
  - list 的 prefix 扫描预分配。
- 验收：读 RPS 打到 GOMAXPROCS 饱和（loopback ≥ 500k QPS）；10k watcher 下 watch E2E p99 ≤ 50ms；`consistent=true` 延迟 ≤ 2× stale。
- 依赖：P2。

**P4 — 基准发布 & 文档（5 人天）**

- 交付：
  - `docs/benchmarking-v1.md`：方法论 + 可复现 CLI + 硬件规格。
  - `bench/releases/v1.0.md`：etcd、Consul、Apollo、PaladinCore 同硬件同 workload 对比。
  - README 头版数字更新，每个数字链接 raw JSON（JSON 留 repo 内，不建公开 dashboard）。
  - bench baseline 提交到 `bench/baselines/` 作为回归基准。
- 验收：三人在三台不同 Linux 服务器上独立跑文档，得出 ±10% 内结果。
- 依赖：P0–P3。

**P5 — v0.9 preview release（2 人天，原 v1.0 release gate 降级）**

- **Eng-review 调整（OV7）**：v1.0 必须在 **P6 chaos 清 0 违反之后**才发，避免发布后被 chaos 反打脸。故本阶段降级为 v0.9 preview，给外部用户探索用；v1.0 tag 在 P6 完成后打。
- 交付：
  - 发布 checklist（LOC ≤ 6000、bench ≥ 目标、goleak pass、静态检查 clean）。
  - CHANGELOG.md 和 Release 文案（明确标注 "preview，chaos 验证进行中"）。
- 验收：所有 checklist 项绿灯；打 tag `v0.9`。
- 依赖：P0–P4。
- 风险：preview 用户反馈严重 bug，需要调整优先级；缓解：版本文案明确 stability tier。

**P6 — 混沌/正确性验证（15 人天，release gate）**

- **Eng-review 调整（OV7）**：本阶段产出是 v1.0 release 的**硬 gate**——`paladin_chaos_test_linearizability_violations_total` 首次归零之前不打 v1.0 tag。
- 交付：
  - `chaos/` 目录：toxiproxy 部署脚本 + scenario runner + history recorder。
  - `chaos/harness/topology.go`（契约 C4，P6 第 1–2 天交付）。
  - `chaos/scenarios/`：至少 10 个 YAML 场景（partition × 4、clock skew × 2、slow disk × 2、node-kill × 2）。
  - porcupine 集成：PaladinCore KV 的 model spec（`chaos/model/config_kv.go`）+ 模型自身的单测（`chaos/model/config_kv_test.go`，喂 known-good / known-bad history）。
  - CI 集成：per-PR quick suite（3 scenario、15 分钟）；nightly full suite（10 scenario、4 小时）。
  - 发布报告 `docs/chaos-report-v1.md`：记录每个 scenario 的通过情况。
  - **v1.0 tag**：chaos 清 0 后打。
- 验收：连续 30 天 CI 中 `paladin_chaos_test_linearizability_violations_total` 为 0；full suite 首次跑通 100% pass。
- 依赖：P2 完成（写路径稳定才有意义）；P5（v0.9 preview 已出，用户已试用过基础功能）。
- 风险：首次跑 full suite 可能暴露既有 bug；缓解：预留 5 人天作为修复缓冲。

**P7 — 技术文章系列（5 篇精选，10 人天）**

- 交付 5 篇 markdown 长文 + 配套 demo 目录：
  1. `docs/articles/01-workload-shape.md` — 配置中心 workload 为何决定架构（覆盖决策 2）。
  2. `docs/articles/02-why-raft.md` — Raft 为什么比 Paxos/gossip 更合适（覆盖决策 7）。
  3. `docs/articles/03-delay-decomp.md` — 写一次 540ms 的延迟分解（覆盖决策 1 和 P0 产出）。
  4. `docs/articles/04-group-commit.md` — BatchProposer 的物理原理与代码实现（覆盖决策 5）。
  5. `docs/articles/05-fsync-dominated.md` — 从 fsync 说起：持久性 vs 吞吐（覆盖 P2 优化）。
- 每篇 2k–4k 字，配一个可独立跑的 demo（`docs/articles/*/demo/`）。
- 验收：5 篇全部完成并 cross-link；每篇结尾附 "进一步阅读" 指向既有 `production-refactoring.md` 和 `day1-7.md`。
- 依赖：P0–P4 完成（要有实测数据才能写）。

**P8 — gRPC Watch 一等公民（10 人天）**

- 交付：
  - `proto/paladin/v1/watch.proto`：WatchService 定义。
  - `server/grpc_server.go`：gRPC server + WatchService 实现，wire 进 `PaladinServer`。
  - `sdk/grpc_watch.go`：SDK 的 gRPC streaming watcher；与既有 HTTP long-poll watcher 共用上层 API。
  - WatchCache ring buffer 扩展到 1024/namespace（原 4096/全局 拆分）。
  - slow-consumer 与 gap 检测（决策 9/11）的服务端 + SDK 实现。
  - 新 metrics：`paladin_grpc_active_streams`、`paladin_grpc_events_sent_total`、`paladin_grpc_slow_consumer_kicks_total`、`paladin_watchcache_gap_forced_fullpull_total`。
- 验收：
  - gRPC watch E2E p99 ≤ 30ms @ 10k concurrent stream。
  - slow-consumer 触发 `RESOURCE_EXHAUSTED` 覆盖单元测试。
  - `ErrGapTooLarge` 自动 fallback fullPull 的集成测试。
  - LOC 不超 6000。
- 依赖：P3 完成（WatchCache 已无锁）。
- 风险：gRPC 引入 ~1000 行挤占其他预算；缓解：先跑 `make loc-budget` 确认剩余预算。

**总计：66 人天（约 8 个 sprint / 人类对等 4–5 个月专注投入）。**

**阶段依赖图（eng-review 修订，OV7）**：

```
P0 ──▶ P1 ──▶ P2 ──▶ P3 ──▶ P4 ──▶ P5 (v0.9 preview) ──▶ P6 ──▶ v1.0 tag
              │       │                                   │
              │       └──▶ P8 (gRPC) ─────────────────────┘   (与 P6 并行；v1.0 含 gRPC)
              │
              └──▶ P7 (文章) 与 P3+ 并行

v1.0 release 的硬 gate = P6 chaos 清 0 违反。
```

### 4.2 优先级矩阵

| 优先级 | 项目 | 价值 | 成本 | 决策 |
|---|---|---|---|---|
| 🎯 | bench CI 闭环 | 高（结构性防御） | 低 | P0 必做 |
| 🎯 | LOC 硬上限（≤ 6000） | 高（防 Creep） | 低 | P0 必做 |
| 🎯 | 延迟分解表 | 高（防 Theatre） | 低 | P0 必做 |
| 🎯 | JSON → proto（内部 wire） | 高 | 低 | P1 必做 |
| 📈 | BatchProposer | 极高 | 中 | P2 必做 |
| 📈 | ReadIndex 线性化读 | 中 | 中 | P3 做 |
| 📈 | WatchCache 无锁发布 | 中 | 中 | P3 做 |
| 📈 | **gRPC Watch 一等 API** | 高（fanout 10×） | 中 | **P8 必做**（CEO review 推翻原 Non-Goal） |
| 📈 | **Jepsen 等价混沌测试** | 极高（正确性可信） | 中 | **P6 必做** |
| 📈 | **5 篇技术文章** | 高（叙事深度） | 中 | **P7 必做** |
| ⚡ | `sync.Pool` hot path | 低 | 低 | P1 顺手 |
| ⚡ | 路径 parse 零 alloc | 低 | 低 | P1 顺手 |
| ❌ | Multi-raft 分片 | 高（但破坏 lightweight） | 极高 | 不做（见第九章 Limitations 论证） |
| ❌ | 公开 benchmark dashboard | 中（营销） | 中 | 不做（rigor 向内，不向外） |
| ❌ | MVCC | 低（配置场景无需） | 极高 | 不做 |
| ❌ | RBAC / auth | 中 | 高 | 不做（应用层自理） |
| ❌ | 替换 hashicorp/raft | 中 | 极高 | 不做 |
| ❌ | 替换 bbolt | 中 | 极高 | 不做 |

---

## 五、成功标准与度量

### 5.1 核心 KPI

| KPI | 定义 | 基线（v0） | 目标（v2.0） | 采集 | 频率 |
|---|---|---:|---:|---|---|
| 写 RPS @ conc=64, p99≤50ms | `bench/scenarios/write_only` 拐点一步下 | ~55 | **≥ 3000** | bench quick-suite | 每 PR |
| 读 RPS @ conc=64, p99≤5ms | `bench/scenarios/read_only` 拐点一步下 | ~94k | **≥ 200k** | bench quick-suite | 每 PR |
| 混合 RPS @ 95/5, conc=64 | `mixed` scenario | ~1.5k | **≥ 30k** | bench quick-suite | 每 PR |
| 非测试代码 LOC | `find . -name '*.go' \! -name '*_test.go' \| xargs wc -l` | ~3800 | **≤ 6000** | CI `make loc-budget` | 每 PR |
| Watch E2E p99 @ 10k watcher | PUT→applyEvents（gRPC 路径） | ~30s（poll 上限） | **≤ 30ms** | bench `watch_fanout` | 每发布 |
| gRPC Watch fanout 容量 | 10k 并发 stream 下丢流率 | 未测 | **≤ 0.1%** | bench `watch_fanout_grpc` | 每发布 |
| Leader failover 写不可用窗口 | bench `failover` | ~2s | **≤ 1s** | bench `failover` | 每发布 |
| etcd 同 workload 比 | PaladinCore/etcd 于 `mixed` 下的 RPS 比 | 未测 | **≥ 50%（写） / ≥ 100%（读）** | `scripts/bench-vs-etcd.sh` | 每发布 |
| bench 可复现性 | 三台独立机器偏差 | 未测 | **≤ ±10%** | 发布前人工 | 每发布 |
| **Linearizability 违反数** | chaos full suite 中 porcupine 报告的违反次数 | 未测 | **= 0** | CI `paladin_chaos_test_linearizability_violations_total` | 每 nightly |
| **Chaos scenario 覆盖** | 每发布必过的 scenario 数 | 0 | **≥ 10** | `chaos/scenarios/` 计数 | 每发布 |
| **技术文章交付数** | P7 精选章节 | 0 | **= 5** | `docs/articles/*.md` 计数 | 每发布 |

### 5.2 健康度监控

线上持续监控（基于已有 `internal/metrics`）：

- **性能**：`paladin_http_request_duration_seconds`（p50/p99）、`paladin_raft_apply_duration_seconds`、`paladin_store_revision`（三节点 diff = 复制 lag）。
- **容量**：`process_open_fds`、`go_goroutines`、`go_memstats_heap_inuse_bytes`。
- **稳定**：`paladin_raft_apply_total{outcome}` 错误率 < 0.1%；`paladin_http_requests_total{status=~"5.."}` 占比 < 0.01%。
- **SDK**：`paladin_sdk_full_pulls_total{outcome}`、`paladin_sdk_revision` vs server `paladin_store_revision` diff（越小越好）。
- **gRPC Watch**（P8 新增）：
  - `paladin_grpc_active_streams{namespace}` — gauge，活跃 stream 数。
  - `paladin_grpc_events_sent_total{namespace}` — counter，累计发送事件数。
  - `paladin_grpc_slow_consumer_kicks_total{namespace}` — counter，被踢的慢消费者数（健康值应 ≈ 0）。
  - `paladin_watchcache_gap_forced_fullpull_total{namespace}` — counter，因 ring gap 触发的 fullPull 次数。
- **Chaos CI**（P6 新增，仅测试环境产出）：
  - `paladin_chaos_test_linearizability_violations_total` — counter，必须长期为 0。
  - `paladin_chaos_test_scenarios_passed_total` / `..._failed_total` — 分 scenario 统计。

告警阈值写 `docs/slo-alerts.md`，不在本文内详展开。

---

## 六、风险登记册

| 风险 | 概率 | 影响 | 缓解 | 触发指标 | 应急 |
|---|---|---|---|---|---|
| bench CI 拖慢 PR 合并 | 中 | 中 | quick-suite 10min 上限；超时自动降级 warn | CI 耗时 > 20min | 拆 quick/nightly 双轨 |
| protobuf wire 跃迁引入 bug | 中 | 高 | 发布前完整 raft compatibility 测试；灰度 | 集群写错误率 > 0.1% | 回滚 wire 版本 |
| BatchProposer 窗口参数失真 | 高 | 中 | A/B 测试选拐点；自适应 | P99 > 100ms 持续 10min | 回退为 1ms 固定 |
| LOC 硬限触发大重构 | 中 | 低 | 每 PR 主动评估预算剩余 | 余量 < 200 行 | 砍功能 |
| etcd 对比结果不如预期 | 中 | 高（叙事） | 重新锚定为"配置中心 workload 特化"而非 raw RPS | 写 RPS ratio < 30% | 改 narrative；保留工程目标 |
| hashicorp/raft 库停更 | 低 | 极高 | 关注 issue；准备 fork 应急 | 库 6 个月无 release | fork 或切 dragonboat（v3 级决策） |
| macOS 开发与 Linux 生产数字差 >2x | 高 | 中 | 所有公开数字强制 Linux | 本地 bench 与 CI Linux 差 > 2× | 文档明示 macOS 仅开发 |
| 大 value 上传 DoS | 中 | 中 | 硬限 256KB + `MaxBytesReader` | value > 256KB 请求数 > 10/min | ban 源 IP |
| 首次 chaos full suite 暴露既有 bug | 高 | 中 | P6 预留 5 人天修复缓冲 | 任一 scenario 报 linearizability 违反 | 优先修复；必要时延后 P5 release |
| porcupine 校验太慢拖垮 CI | 中 | 中 | quick suite 场景数量封顶 3；full suite 移到 nightly | 单 scenario 校验 > 60s | 切 nightly；或降低 history 采样率 |
| gRPC 引入 ~1000 行挤占 LOC | 高 | 中 | P8 前先跑 `make loc-budget` 评估；必要时先砍死角代码 | `loc-budget` 余量 < 300 行 | 延后 P8 到 v2.1 或精简非关键模块 |
| gRPC stream kick 率异常高 | 中 | 中 | 监控 `slow_consumer_kicks_total`；客户端默认预热 | kick 率 > 1%/hour | 调大 ring buffer 或重新设计慢客户端 UX |
| 双传输 SDK 维护成本失控 | 中 | 中 | 共享上层 API；测试覆盖双路径 | SDK 代码重复率 > 30% | 抽取共享抽象；或废弃 HTTP/1.1 路径 |
| 技术文章拖延（P7 非强编码任务） | 高 | 低 | 每 sprint 强制交付 1 篇草稿；CEO plan 中列为硬交付 | 4 周内未推进 | 重新评估该文章是否必要 |

---

## 七、禁止事项（Anti-Patterns）

基于阶段三失败考古，以下在 v2 代码库中**硬禁止**，违反者 PR 被拒：

1. **benchmark 数字无 Env fingerprint 发布**——可复现性是底线。
2. **hot path 使用反射 / `encoding/json`**——用 `easyjson` 或 protobuf。
3. **引入通用性能力但配置中心不用**——MVCC 版本链、lease、auth 都是作用域陷阱。
4. **用 `sync.Map` 做"无锁 map"**——不是；对本场景更慢。
5. **hot path `context.Background()` 分配**——改为 request-scoped ctx。
6. **hot path `fmt.Sprintf`**——改 `strings.Builder` 或 `strconv.Append*`。
7. **`err.Error() == "..."` 错误字符串匹配**——用 `errors.Is`；已在 golangci lint 封。
8. **全局单例 Registry（除 `internal/metrics` server 侧已约定外）**——SDK 等库必须 per-instance。
9. **`FSM.Apply` 里做阻塞 >1ms 的事**——整条 Raft 挂死。
10. **把大 value 当一等功能**——硬限 256KB，超限 413。
11. **功能 + 优化混合 PR**——各自独立，优化必附前后基线。
12. **公开数字来源 macOS 或 loopback**——必须 Linux + 真网络。
13. **benchmark headline 基于 fsync=off**——SYNTHETIC 标签不得出现在主数字位。
14. **超过 LOC 上限的 PR 不 delete 同等代码**——代码预算是零和。
15. **发布时 `paladin_chaos_test_linearizability_violations_total` > 0**——正确性违反一律 blocker，不可用"已知 flaky" 理由绕过。
16. **跳过 chaos CI 发 release**——任何 v1.x+ release 都必须附 `docs/chaos-report-v*.md`；无则不发。
17. **gRPC Watch slow-consumer 静默吞事件**——必须 kick 流 + 返回明确 status code（决策 9）；禁止静默丢事件或阻塞 broadcast。
18. **在 `FSM.Apply` 直接调用 gRPC stream 或做网络 IO**——保持 FSM.Apply 无阻塞；事件向 WatchCache 发布，流层独立 goroutine 消费。

---

## 八、错误 / 鉴救映射表（Error & Rescue Map）

下表枚举 v2 新增执行路径下所有**可预见**的失败模式及其鉴救路径。任何在此表之外产生的错误都是 bug。

| 场景 | 失败表现 | 客户端观察 | 服务端处理 | SDK 鉴救 | 测试归属 |
|---|---|---|---|---|---|
| Raft quorum 丢失（多数节点挂） | 写无法 commit | `504 Timeout`（HTTP）/ `UNAVAILABLE`（gRPC），> 10s | Leader 等 commit 超时后 fail 该批 | 调用方退避重试；必须准备好 "unknown outcome" | P6 scenario `quorum_loss` |
| Leader 被网络分区孤立 | 写被老 leader 接受但无法提交 | 最终 `ErrNotLeader` | 老 leader 收不到 follower ack，heartbeat 超时后自降级 | SDK 刷新 leader map + 重试 | P6 scenario `leader_isolated` |
| 时钟跳变 > 30s | Raft heartbeat 异常 → 偶发选举抖动 | 短暂写不可用（< 5s） | Raft 自愈 | 透明重试 | P6 scenario `clock_skew` |
| gRPC Watch slow consumer | Stream 被关 | `RESOURCE_EXHAUSTED` | 服务端 close stream + 指标 +1 | SDK 用最后 revision fullPull 后重建 stream | P8 unit + P6 `slow_watcher` |
| WatchCache ring gap（SDK 长时间离线） | Stream 建立失败 | `FAILED_PRECONDITION` + `ErrGapTooLarge` | 返回错误 + 服务端指标 +1 | 自动 fullPull 后再订阅 | P8 integration test |
| Server 重启 / 滚动升级 | 现有 streams 全部断开 | `UNAVAILABLE` | 正常关闭 | SDK 透明重连；若 revision 已滚出 ring 触发 fullPull | P6 scenario `rolling_restart` |
| bbolt 磁盘满 / fsync 失败 | 写 commit 失败 | `500 Internal`（HTTP）/ `INTERNAL`（gRPC） | FSM.Apply 返回 error；Raft 不标记已 applied | 退避重试；运营告警触发人介入 | P6 scenario `disk_full` |
| gRPC Watch stream 建立后连接中断 | 客户端收到 EOF | `UNAVAILABLE` | 服务端释放资源 | SDK 透明重连 | P8 integration test |

---

## 九、Limitations — 被拒绝的扩展与理由

CEO review 过程中明确拒绝的扩展，在此留下完整论证，供未来重新评估。

### 9.1 不做 Multi-Raft 分片

**问题本质**：配置中心场景是否需要把 key space 按 namespace 拆成多个独立 Raft group，以突破单 Raft 写吞吐 ~3 万/s 的天花板？

**拒绝理由**：

1. **违反 lightweight 北极星**：multi-raft 至少新增 ~1500 行（group 路由、shard 再平衡、跨 shard 一致性边界），LOC 预算不够。
2. **典型配置中心写 RPS 远低于 3 万/s**：Apollo / Nacos 报告的典型生产写 RPS 通常 < 500/s（配置变更是稀疏事件）。"beat etcd on write RPS" 不是真实用户需求。
3. **复杂度外溢代价不对等**：multi-raft 引入跨 shard 事务（或显式拒绝跨 shard 原子性），推高 SDK / 运营复杂度；而对 config-center workload 几乎零收益。
4. **与项目气质冲突**：multi-raft 是"用架构复杂度刷数字"的典型动作；v2 立场明确拒绝此类动作。

**成立的价值权衡**：接受写吞吐 ~3 万/s 封顶；换取代码清晰、部署简单、单集群诊断心智模型稳定。

**重新评估触发条件**：
- 实测有用户使用场景写 RPS > 1 万/s 持续超过 1 周；或
- 单 Raft 在发布后实测写瓶颈不在 bbolt/fsync 而在 Raft 协议本身（此时 multi-raft 才能解）。

### 9.2 不做公开连续 benchmark dashboard

**问题本质**：是否应该搭 GitHub Pages 展示每次 commit 的 benchmark 曲线，作为 perf 可信度的对外信号？

**拒绝理由**：

1. **rigor 向内不向外**：v2 气质是"让审视者信服"，不是"让观众印象深刻"。dashboard 属后者。
2. **维护负担**：CI token、部署触发、数据保留策略都是长期小毛刺，分散核心精力。
3. **早期数据难看**：P0-P2 期间数据还在演进，过早公开可能吓走第一批读者。
4. **替代方案已足**：bench baseline JSON 留在 repo `bench/baselines/`；每次 PR CI 都 diff 本次 vs baseline；想深究的读者 `git log bench/` 即可。

**重新评估触发条件**：项目获得实质 traction（GitHub stars > 1k 或外部贡献者 ≥ 5）后，再评估是否需要对外 dashboard。

### 9.3 不做 Jepsen 原生 (Clojure) 引入

**问题本质**：是否应该用 Jepsen（Clojure/JVM）而不是 porcupine+toxiproxy 这条 Go-native 路径？

**拒绝理由**（见决策 10 详细论证）：
1. 违反 Go-native lightweight 约束（引入 JVM + Clojure 工具链）。
2. 新贡献者门槛 +50%（必须会两种语言两种生态）。
3. porcupine + toxiproxy 覆盖 80% 的 Jepsen 能力，对 config-center workload 已足够。

**接受的代价**：放弃"Jepsen-verified" 的营销标签；声称 "linearizability verified via porcupine + toxiproxy" 即可，后者在学术上同样被接受。

---

## 附录 A：术语表

- **Workload 特化**：在设计决策中显式利用特定工作负载性质（读写比、值大小、key 密度）做简化，接受该范围外的非最优。
- **延迟分解表（latency decomposition）**：把单请求耗时按处理阶段拆到 ns 级的表格；优化前后对比表是 PR 的标准附件。
- **拐点（knee point）**：RPS-p99 曲线上，RPS 增长停止而 p99 开始急剧上升的并发点；"安全 RPS"取拐点下一档。
- **group commit**：DB/存储系统中把并发写批量合并为一次 fsync 以摊薄成本的技术。本文移植到 Raft Apply。
- **ReadIndex**：etcd/Raft 术语，让读请求等到集群 commit index 追上本地 apply index 再返回，实现线性化读。
- **Env fingerprint**：benchmark 结果中记录的硬件/OS/Go 版本/GOMAXPROCS 等，决定两份结果是否可比。
- **Linearizability**：最强的并发一致性保证——每个操作看起来像在 invoke 和 response 之间某个瞬时点原子执行，且全局呈现单一顺序。Raft 正确实现的 CP 目标。
- **Chaos harness**：一套主动注入网络分区/时钟跳变/节点崩溃并记录系统响应的测试框架。本文采用 porcupine + toxiproxy 组合。
- **porcupine**：MIT 开发的 Go linearizability checker 库，输入 history（一串 `(invoke, response)`），输出是否存在线性化排序。
- **toxiproxy**：Shopify 开源的 TCP 代理层，可主动引入延迟/断开/丢包，常用作混沌测试的网络侧载体。
- **rigor over marketing**：本项目气质——选择更严谨/更可审计/更诚实的路径，即使 headline 数字不那么漂亮。

## 附录 B：参考资料

- etcd Raft implementation notes & benchmarks — <https://etcd.io/docs/>
- hashicorp/raft library — <https://github.com/hashicorp/raft>
- bbolt — <https://github.com/etcd-io/bbolt>
- "Building a Distributed Log"（Jack Vanlightly）——系统地解释 Raft pipeline 与 batching
- Redis single-writer design notes——简单工作负载下避免锁的典型样本
- ZooKeeper Zab paper — 流水线化共识的经典参考
- porcupine linearizability checker — <https://github.com/anishathalye/porcupine>
- Anishathalye, "Porcupine — A fast linearizability checker" (MIT blog) — porcupine 设计与 history 建模思路
- toxiproxy — <https://github.com/Shopify/toxiproxy>
- Kyle Kingsbury (aphyr), Jepsen reports on etcd / Consul / ZooKeeper — 混沌测试的业界标杆
- gRPC streaming best practices — <https://grpc.io/docs/languages/go/basics/>
- MIT 6.824 Distributed Systems Lab (Raft + linearizability) — 教学级参考实现坐标
- 本仓库现有 `docs/production-refactoring.md`——全量生产化蓝图，与本文互补
- 本仓库 `docs/day1.md` ~ `docs/day7.md`——教学化的从零构建系列，是 P7 技术文章的种子

---

**本文档本身受 LOC 与复杂度约束的精神约束**：若读者读完后仍不知道本周该做什么，说明文档失败；若超过 2 小时能读完，说明文档冗余。

---

## GSTACK REVIEW REPORT

| Review | Trigger | Why | Runs | Status | Findings |
|--------|---------|-----|------|--------|----------|
| CEO Review | `/plan-ceo-review` | Scope & strategy | 1 (2026-04-21) | CLEAR | 5 scope expansions proposed, 5 accepted (E1/E3/E4/E5/E6 via CEO plan); 4 deferrals written to TODOS.md |
| Codex Review | `/codex review` | Independent 2nd opinion | 0 (tool failed — MCP config issue) | — | Fell back to in-session fresh-eyes self-critique; 10 outside-voice concerns surfaced |
| Eng Review | `/plan-eng-review` | Architecture & tests (required) | 1 (2026-04-21) | ISSUES OPEN | 15 issues (6 architecture + 3 code-quality + 3 perf + 3 test-flow); all 12 inline decisions locked; 2 critical durability gaps tracked in TODOS.md (OV3, OV4) |
| Design Review | `/plan-design-review` | UI/UX gaps | 0 | — | N/A (backend infra project, no UI surface) |

**CODEX:** Tool failed on this machine (`missing field command in mcp_servers.figma`). Fresh-eyes self-critique used as substitute; reduced-confidence signal noted.

**CROSS-MODEL:** N/A — outside voice was self-generated. Re-run `/codex review` after fixing MCP config for a true second opinion.

**UNRESOLVED:** 0 decisions left unresolved (all 12 inline decisions locked). 4 outside-voice concerns (OV3/OV4/OV9/OV10) tracked in `TODOS.md` as open questions with explicit re-evaluation triggers.

**CRITICAL GAPS:** 2 (flagged in §八 Error Map and TODOS.md):
1. Pre-v2 Raft log replay — **mitigated** in plan via contract C1 + regression test requirement; realized-risk reduces to 0 at implementation.
2. Crash-consistency / fsync-ordering durability — **open** (OV3). Porcupine cannot detect this class; narrower claim or new tooling needed before v1.0 release notes.

**LAKE SCORE:** 12/12 eng-review inline decisions chose the complete option (A). 4 outside-voice concerns deferred to TODOS.md for explicit re-evaluation (not shortcuts).

**VERDICT:** CEO + ENG CLEARED for implementation. P0 kickoff is unblocked. v1.0 release gated on P6 chaos suite clean (OV7 reordering). Durability narrative (OV3) must be resolved before v1.0 release notes ship.
