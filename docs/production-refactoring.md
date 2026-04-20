# PaladinCore 生产级重构蓝图

> 一份把 PaladinCore 从"能跑 demo 的教学艺术品"演化为"能承担 10k 量级 SDK、5k QPS 写、500k QPS 读、四个九可用性"的结构化重构文档。
> 目标读者：2–3 人架构小组。
> 风格：不谈愿景，只谈手术动作、取舍成本、验收标准。每一条都能变成 sprint 板上一张可落地的卡。

---

## 目录

- [0. 执行摘要](#0-执行摘要)
- [1. 现状评估](#1-现状评估)
- [2. 重构原则与不变量](#2-重构原则与不变量)
- [3. 核心架构重构：八大主题](#3-核心架构重构八大主题)
- [4. 生产能力补强](#4-生产能力补强)
- [5. 性能优化路线图](#5-性能优化路线图)
- [6. 分阶段落地计划](#6-分阶段落地计划)
- [7. 风险矩阵与缓解](#7-风险矩阵与缓解)
- [8. 测试与验证策略](#8-测试与验证策略)
- [9. 附录](#9-附录)

---

## 0. 执行摘要

### 0.1 一句话定位

把 PaladinCore 重构——不重写——到一个小型生产配置中心的可用区间。**以压测数字说话，每一次改动都要被基线量化。**

### 0.2 关键 SLO 目标

| 维度 | 基线（3-node localhost 实测） | 目标（v1.0） | 倍数 |
|---|---:|---:|---:|
| 写 RPS @ p99 ≤ 50ms | ~20 | **5 000** | 250× |
| 读 RPS @ p99 ≤ 5ms | ~90 000 | **500 000** | 5× |
| 单集群并发 SDK 长连接 | 未测 | **10 000** | — |
| Watch E2E p99 | ~30s（长轮询上界） | **≤ 200ms** | — |
| Leader failover 写不可用窗口 | ~2s | **≤ 800ms** | 2.5× |
| 集群可用性（3 voter） | — | **99.99%** | — |
| 10 GB 冷启动恢复 | 未实现 | **≤ 60s** | — |

### 0.3 工作量粗估（约 18 人周）

| 阶段 | 主题 | 人周 | 产出 |
|---|---|---:|---|
| P0 地基 | config / slog / metrics / 错误码 / proto 骨架 | 3 | 无 perf 收益，解锁一切 |
| P1 写路径 | ApplyBatch、group fsync、pb wire、snapshot 重写 | 4 | 写 RPS 拉到 5k |
| P2 读/Watch/SDK | ReadIndex、Watch gRPC stream、SDK 多路 | 3 | 读侧 SLO 达标、Watch 支撑 10k 连接 |
| P3 API 扩展 | Lease、Txn (CAS)、Compact、Defrag | 3 | 能替代 80% etcd 场景 |
| P4 安全与运维 | TLS/mTLS、RBAC、审计、backup/restore、滚动升级 | 3 | 可上生产 VPC |
| P5 稳定化 | 混沌、长跑、灰度、文档 | 2 | 发 v1.0 |

### 0.4 假设（不成立则本文档需局部重写）

1. 规模：单集群 ≤ 50 GB、≤ 10M keys、≤ 10k 并发 SDK。超出需引入分片层。
2. 允许**一次**wire 跃迁（JSON→protobuf），之后严格前后兼容。
3. 不做多 DC 复制、不做 gossip 成员发现；依然是单 Raft group。
4. 生产介质 NVMe + Linux；macOS 仅开发用。

---

## 1. 现状评估

### 1.1 必须保留的价值（重构不要误伤）

| 资产 | 为什么保留 |
|---|---|
| 五层窄接口（`store → WatchableStore → raft.FSM → server → sdk`） | 可逐层替换不污染邻层——结构化重构可行的根本 |
| etcd 风 Revision 语义 | 心智模型已验证；对齐 etcd 免重造 |
| 透明 Leader 转发 | 客户端拓扑无感，接口不变 |
| Ring buffer watch 模型 | 语义正确，只是性能短板，不要推翻 |
| SDK 三段式（拉取→长轮询→SHA256 缓存） | 精华，只强化不重造 |
| `@bench/` 工具 + 基线 | 每步 perf 改动可量化——"以数据说话"的前提 |

### 1.2 生产级差距清单（按严重度）

分三维：**能力**（功能）、**质量**（健壮/安全）、**规模**（性能/运维）。

| 严重度 | 维度 | 差距 | 代码锚点 |
|---|---|---|---|
| 🔴 | 质量 | 错误协议是字符串比较（`err.Error() == "apply error: key not found"`）——文案微调即崩 | `@server/raft_server.go:122` |
| 🔴 | 质量 | Raft `Op` JSON 序列化，无 schema version；字段重命名 = 集群数据语义断裂 | `@raft/node.go:32-36` `@raft/node.go:143` |
| 🔴 | 规模 | FSM.Apply 每条 log 独立 bbolt TX + 独立 fsync，**无 batch** | `@raft/node.go:291-316` |
| 🔴 | 规模 | `FSM.Snapshot` = `List("")` 全量进内存 + JSON，10 GB 直接 OOM | `@raft/node.go:319-327` `@raft/node.go:361-375` |
| 🔴 | 能力 | 无鉴权、无 TLS、无审计；生产网络不可暴露 | `@server/server.go` |
| 🟠 | 能力 | 无 Lease/TTL、无 Txn/CAS；所有协调原语无法用 Paladin 承担 | — |
| 🟠 | 能力 | 无 `ReadIndex`；客户端线性一致读无路可走 | `@raft/node.go:165-168` |
| 🟠 | 规模 | Watch 每次长轮询 O(N) 扫环形缓冲；>4k 并发 SDK 锁争用爆炸 | `@store/watch.go:139-150` |
| 🟠 | 规模 | Watch 每次 `WaitForEvents` 分配 goroutine + timer；GC 压力 | `@store/watch.go:105-120` |
| 🟠 | 质量 | 慢 watcher **静默驱逐**——客户端无法区分"没事件"与"丢了" | `@store/watch.go:62-72` |
| 🟠 | 规模 | Follower 转发用 `http.DefaultClient`，无池调优无超时 | `@server/raft_server.go:176` |
| 🟡 | 能力 | SDK 只用 `Addrs[0]`，任一节点掉线不切 | `@sdk/client.go` |
| 🟡 | 质量 | 无 structured logging / trace_id / metrics endpoint | 全局 |
| 🟡 | 质量 | 只有 CLI flag，无配置文件，无热更新 | `@cmd/paladin-core/main.go` |
| 🟡 | 质量 | 无 backup/restore；"`cp data.db`"在活跃写时会坏数据 | README Non-Goals |
| 🟡 | 规模 | Snapshot.Restore 逐条 Put，10M keys 启动数分钟 | `@raft/node.go:341-345` |
| 🟡 | 质量 | FSM.Apply Put 错误分支写了"unmarshal op"文案（copy-paste bug） | `@raft/node.go:301` |
| 🟢 | 质量 | `raft.DefaultConfig()` 未调；heartbeat/election timeout 用默认 1s，抖动下频繁 failover | `@raft/node.go:79-82` |

### 1.3 压测证据（最具说明力的三条）

3-node localhost，c=16，500ms warm-up + 3s 窗口：

| 场景 | RPS | p50 | p99 | 解读 |
|---|---:|---:|---:|---|
| `write_only` | **55** | 221ms | 541ms | **已达 APFS 单线程 fsync 上限** |
| `read_only` | 94 335 | 150µs | 550µs | 读侧远未饱和 |
| `mixed_95r_5w` | 1 566 | 250µs | 174ms | **5% 写拖垮 99% 分位** |

→ 优先级直接锁定：**写路径（主题 B）第一个上**。

---

## 2. 重构原则与不变量

### 2.1 九条硬约束（违反即拒 PR）

1. **Wire 格式向后兼容。** pb schema 一旦落地只加字段、tag 单调递增，不重命名、不改类型。
2. **语义向后兼容。** `Revision` 族语义在 v1.0 后冻结。
3. **错误码向后兼容。** 发布后只加不改。
4. **数据格式向后兼容。** BoltDB bucket + snapshot 都有版本号，支持只读旧版。
5. **Raft log 格式演进 = 一次 compaction。** 任何 wire 跃迁搭配全集群 snapshot + log truncate，不允许混合 log 持久存在。
6. **所有新代码 race-free。** `go test -race` 常绿。
7. **所有公开 API 有 metrics / tracing hook。** 没有 observability 的代码不 merge。
8. **对外接口必须有显式超时。** 没有超时 = bug。
9. **Go idiomatic + 业界最佳实践。** 新代码须通过严格 linter 套件（`gofumpt` / `go vet` / `staticcheck` / `golangci-lint` / `goleak` / `govulncheck`）；不仅遵循《Effective Go》《Uber Go Style Guide》《Google Go Style Guide》，还须与 **etcd / CockroachDB / HashiCorp Consul / Kubernetes / Prometheus / gRPC-Go** 等一线开源项目的常见处置对齐——凡偏离，须在 PR 描述里论证。详细规则见 §2.3。

### 2.2 语义不变量（不能破坏）

- 单调递增全局 `Revision`。
- `PUT` 成功后，返回的 Revision 在**任意节点**后续 GET 中可见（容忍 apply lag）。
- 删除是第一类操作（`DELETE` event 带 `PrevEntry`），不是"写空"。
- Watch 永不丢事件 **或** 显式 `compacted=true`（不再静默丢）。
- Leader 不可用期间，**写**返回 `LEADER_UNAVAILABLE`；**读**不受影响。

### 2.3 代码风格与工程规范

默会知识必须显式化——否则每次 code review 会退化成口味战争。《Effective Go》《Uber Go Style Guide》《Google Go Style Guide》给出基线；真正的功力在一线开源项目的**具体处置**里——它们已经踩过我们即将踩的每一个坑，并在 commit history 里留下了答案。本节把这些共识固化成 PR 可检查的清单。CI 能守的由 CI 守（§H），守不住的由 reviewer 守。

**参考坐标系（按问题查，不必通读）**：

| 问题类别 | 首选参考工程 | 具体看 |
|---|---|---|
| Raft FSM / snapshot / 成员变更 | **etcd v3**、**HashiCorp Consul / Nomad**（同用 `hashicorp/raft`） | `etcd/server/etcdserver/apply.go`、`consul/agent/consul/fsm/*` |
| 分布式 KV + 线性一致读 | **CockroachDB** | `pkg/kv/kvserver/replica_read.go`、`pkg/util/hlc` |
| Context 传播 + 生命周期 | **Kubernetes** `client-go` | `client-go/tools/leaderelection`、`apimachinery/pkg/util/wait` |
| Lease / Watch 语义 | **etcd v3** | `server/lease`、`server/mvcc/watchable_store.go` |
| gRPC interceptor / 错误体系 | **gRPC-Go**、`grpc-ecosystem/go-grpc-middleware` | `status`、`recovery`、`retry` interceptor |
| Metrics 命名与 histogram | **Prometheus**、**VictoriaMetrics** | `prometheus/client_golang/prometheus/*` |
| 存储引擎、零拷贝、TX 生命周期 | **bbolt**、**Pebble**（CockroachDB） | 尤其 bbolt `README.md` "Caveats" 章节 |
| 错误包装与分类 | **CockroachDB** `cockroachdb/errors`、**PingCAP** `pingcap/errors` | 支持 stack trace + 结构化 wrap |
| 优雅退出 | **containerd / Docker** | `pkg/shutdown`、`cmd/*/main.go` 的信号处置 |
| 配置系统 | **Caddy**、**Vault** | 热 reload、zero-downtime |

---

#### A. 命名、文件与包组织

- 包名**短、小写、单数**，不要驼峰、不要下划线。反例：`utils / common / helpers / misc`——这些是**设计失败的产物**（Kubernetes 已公开反思 `pkg/util` 的病）。有凝聚力的名字：`raft`、`store`、`lease`、`watch`（我们现在就是对的，别退化）。
- 每个**导出标识符必须有文档注释**，且以标识符名开头（Go 规范）。`// Why:` 注释只解释设计选择，不解释代码字面——保留代码即文档的信念。
- **一个文件一个职责**；500 行是软阈值，到了先问自己能不能拆（kubernetes、cockroach 项目都有该文化）。
- `internal/` 封装不对外 API——Go 1.4+ 原生机制，不靠约定。
- **禁用 `init()` 做有副作用的事**（etcd 曾因此坑过插件加载顺序）。构造显式 `New...`，依赖注入，不要隐式全局。
- 不要造 `types.go` / `interfaces.go` / `constants.go` 这种"按语法分类"的文件——按**业务领域**切（CockroachDB 明确反对）。

#### B. 错误处理

- **哨兵错误** 用 `var ErrFoo = errors.New("...")`，调用方用 `errors.Is` 比较——**禁止 `err.Error() == "..."`**（§3.8 Theme H 的核心；linter 强制）。
- 包装错误：`fmt.Errorf("apply op %q key=%q: %w", opType, key, err)`——带**可行动**的上下文。借鉴 CockroachDB `errors.Wrapf`：每一层都加语义，不是噪音。
- 复合错误：`errors.Join`（Go 1.20+）或 `hashicorp/go-multierror`——Lease 批量撤销、事务多路失败场景。
- **错误分类**：区分 `transient`（可退避重试）vs `terminal`（抛给调用方）。借鉴 CockroachDB `errors.IsRetriable`、etcd `rpctypes.ErrGRPC*` 分组。
- **Never `panic()` in request path**。只在程序启动时不变量被破坏才 panic（参考 etcd `panic("programmer error: ...")` 的用法）。
- **gRPC handler 的 panic 必须被 interceptor recover** 并转 `codes.Internal` + 告警——直接抄 `grpc-ecosystem/go-grpc-middleware/recovery`。
- 错误 message：**小写开头、无句号、无换行**（Go 规范）。主语是"what failed"，不是"I tried to ..."。

#### C. 并发与 context

- `context.Context` 是**每个 I/O / RPC / DB / Raft 调用的第一个参数**——任何接口方法没 ctx = API bug，不要变通。
- Ctx 派生：`ctx, cancel := context.WithTimeout(parent, X); defer cancel()`——`cancel` 的 defer **永远不能省**，否则泄漏。
- **Ctx 不存 struct field**（Go FAQ 明令；例外：长生命周期 actor 的 `stopCtx`，且必须 `context.Background()` 派生）。
- **Goroutine 必须有 owner + 退出路径**。参考：
  - `golang.org/x/sync/errgroup.Group`——一组 goroutine 的标准容器。
  - `k8s.io/apimachinery/pkg/util/wait.Until`——周期性任务的标准写法。
  - `uber-go/goleak`——测试断言无泄漏。
- 并发原语选择：
  - 关键段用 `sync.Mutex` **零值**；不要 `&sync.Mutex{}` 或指针嵌入。
  - 读多写少 `sync.RWMutex`——但 bbolt 的教训：读锁也不是免费午餐，profile 后再定。
  - 单次初始化 `sync.Once`；池化热对象 `sync.Pool`。
  - 原子小操作用 `atomic.Int64 / atomic.Pointer[T]`（Go 1.19+ typed atomics）——不用老式 `atomic.LoadInt64(&x)`。
- **Channel 协议**：**sender close, receiver range**——永不反过来。关闭已关闭 channel = panic（参考 etcd 多次 bugfix）。
- **不要在 goroutine 里裸丢 err**——回传到 owner 或 `logger.Error` 结构化记录，绝不 `_ = doStuff()`（`errcheck` linter 强制）。

#### D. 接口与依赖

- **"Accept interfaces, return structs"** 是起点但不是教条。**不要预置接口**——先有两个具体实现再抽接口（kubernetes 早期"一切皆 interface"的反思）。
- 小接口优于大接口：`io.Reader` / `io.Writer` 是标杆；`store.Store` 这样的 5–10 方法接口是**上限**。
- **公开 API 不暴露 `any / interface{}`**——编译期类型安全永远优先。泛型（Go 1.18+）适用即用。
- **Functional Options** 模式：`func New(opts ...Option) *T`——借鉴 `google.golang.org/grpc`、`uber-go/zap`、`uber-go/fx`。不要 ctor 堆 15 个参数。
- 避免 "stringly-typed API"——状态用枚举 + `String()`，不要用 raw string 传意图（`"pending" / "done"` 是 code smell）。
- 构造时**显式依赖注入**（wire / 手工），不要隐式全局 singleton（Vault 的 `cores` 管理是好范例）。

#### E. 性能与内存

- **零拷贝 slice**：bbolt `tx.Get` 返回的 slice 在 tx 生命周期外使用 = UB——必须 `append([]byte(nil), src...)` 或 `bytes.Clone`（Go 1.20+）。bbolt README "Caveats" 明确警告。
- **预分配容量**：`make([]T, 0, n)` / `make(map[K]V, n)`——go-perfbook 第一课，CockroachDB hot path 遍布。
- **热路径避免 `defer`**（Go 1.14 后 ~nanoseconds 级，但 Raft Apply 这种每秒万次的循环仍需 profile）；参考 `runtime/mprof.go` 风格：顶层函数无 defer，清理显式写尾部。
- **`strings.Builder` 替代 `+=`**；字节拼接用 `bytes.Buffer` 或 `[]byte` append。
- **`sync.Pool`** 化 hot allocations——proto buffer、`WatchResponse`、`bytes.Buffer`（gRPC-Go 大规模使用）。记住 Pool 不是 cache，GC 可能随时清空。
- **Struct field alignment**：大字段在前、atomic 字段开头确保 8-byte 对齐——`fieldalignment` linter 自动检查（对 64-bit atomic 在 32-bit 平台是**正确性**问题，不是优化）。
- 不要在 tight loop 里 `time.Now()` 做累计——用一次 `start := time.Now()` + `time.Since(start)`。
- 避免反射热路径（`reflect.DeepEqual` 在比较已知类型时换手写）——Prometheus / CockroachDB 都有 benchmark 驱动的换手写案例。

#### F. 测试规范

- **Table-driven tests** 是 Go 社区默认——stdlib / etcd / kubernetes 一致。每个 case 一行 struct，子测试用 `t.Run(name, ...)` 方便 `-run` 过滤。
- **禁止 `time.Sleep` 做同步**——用 channel、条件变量、或 `testify/assert.Eventually`（带超时 polling）。"sleep-based test" = flaky test，是 CI 噩梦的单一最大来源。
- **`goleak.VerifyNone(t)`** 放在 `TestMain` 或关键测试尾——uber 开源，kubernetes / cockroach 广用。
- **Golden file testing** 用于协议契约：`testdata/*.golden`，`-update` flag 再生。stdlib `go/format`、k8s `apiserver` 都是这风格——最适合我们的 pb wire 兼容回归。
- **集成测试独立 build tag**：`//go:build integration`，CI 分层跑（快测 < 1 分钟，集成 < 10 分钟）。
- **Fuzz** 覆盖 parser / codec / watch range 路径（Go 1.18+ native）——etcd、cockroach 都在走。
- **Race detector 常绿** = 约束 #6，但单说一遍：`go test -race ./...` 不绿禁止 merge。
- **不要 mock 得过深**——k8s 的 envtest、etcd 的 `integration/` 都倾向"起真实子系统"而非层层 mock；mock 只 mock 外部网络边界。

#### G. 日志、度量、追踪

- 日志框架：`log/slog`（Go 1.21+ stdlib）。**禁用** `stdlib log.Printf` / `logrus`——前者无结构化，后者 allocation 重。`slog` 足以替代 `zap` 的 95% 场景，stdlib 无依赖。
- **Structured only**：`logger.Info("apply done", "key", k, "rev", r, "dur_ms", d)`，**不要** `log.Printf("apply done key=%s rev=%d", k, r)`——后者无法被日志系统字段化索引。
- 日志级别约定：
  - `Debug` —— 仅在诊断模式；生产默认关。
  - `Info` —— 关键状态变更（leader change、snapshot 完成）。不用 info 打"正常请求"——那是 metric 的职责。
  - `Warn` —— 可自恢复但值得注意（client 重试、ctx deadline 近了）。
  - `Error` —— 需人工介入或影响 SLO；必须可 alert。
- **禁记敏感字段 value**：key 可 log（便于排错），value、token、password 默认脱敏；`--log-values` debug flag 才开（借鉴 Vault 的 "audit log redaction"）。
- **Metric 命名** 严守 Prometheus 最佳实践：`<namespace>_<subsystem>_<name>_<unit>` 全小写下划线。
  - 对：`paladin_raft_apply_duration_seconds`
  - 错：`paladinRaftApplyMs` / `paladin.raft.apply.latency`
- **Histogram bucket** 跟业务分布：延迟多用 `prometheus.ExponentialBucketsRange(0.0001, 10, 20)`；字节数用 `prometheus.ExponentialBuckets(64, 4, 10)`。**不要用 `DefBuckets`**——它是 HTTP 假设。
- **Trace span 命名** `<component>.<operation>`：`raft.apply`、`bolt.tx`、`watch.dispatch`。`traceparent` 通过 gRPC / HTTP interceptor 自动传递，业务代码不手工处理（otelgrpc / otelhttp）。
- 每次打日志 / 记 metric **都要问**："凌晨 3 点 oncall 的人，看到这条能定位到哪行代码吗？"

#### H. 工具链与 CI 强制

| 工具 | 用途 | 强制阶段 |
|---|---|---|
| `gofumpt` | `gofmt` 超集（更严） | pre-commit + CI |
| `go vet` | 标准静态检查 | CI |
| `staticcheck` | bug + style 深度检查 | CI |
| `golangci-lint` | 聚合运行：`errcheck / govet / ineffassign / gosec / revive / gocritic / misspell / bodyclose / contextcheck / errorlint / nilerr / exhaustive / unconvert / unparam / prealloc / fieldalignment / gofumpt` | CI（不绿禁 merge） |
| `goleak` | goroutine 泄漏断言 | `go test` 内 |
| `govulncheck` | 官方 CVE 扫描 | CI + nightly cron |
| `go-licenses` | 依赖 license 合规 | CI |
| `buf lint` | protobuf 风格 + breaking 检测 | CI（wire 改动触发） |
| `benchstat` | bench 数字对比基线 | PR comment 自动贴 |

基线配置：

- `.golangci.yml` 入仓（参考 CockroachDB / kubernetes 的配置做裁剪起点）。
- `.editorconfig` 统一缩进 / 行尾。
- `Makefile` 或 `scripts/lint.sh`：`make lint` 本地一键全跑，等价 CI 的子集。
- PR 模板带 checkbox："☐ `make lint` 本地通过   ☐ 新代码有测试   ☐ 破坏性变更已在 CHANGELOG 标记"。
- **人为豁免**任一 linter rule 须在 `//nolint:rule // reason: ...` 内写理由——CI 检查 "reason" 非空；无理由豁免视同 linter 未过。

---

## 3. 核心架构重构：八大主题

每个主题统一：**现状 / 问题 / 方案 / 收益 / 成本**。

### 3.1 Theme A — Wire 格式稳定化（JSON → protobuf）

**现状**：`Op` 是 JSON struct；FSM Apply 内 `json.Unmarshal` 是热路径 `@raft/node.go:32-36` `@raft/node.go:143`。

**问题**：

- 无 schema 演进机制——加 `TTL` 字段，旧 follower 行为跨 Go 版本不一致。
- c=64 写压测下 JSON codec 占 apply loop ~12% CPU。
- 字段即字符串；mono-repo 外无法引用契约。

**方案**：

1. 引入 `api/paladinpb/v1/`；定义 `OpEnvelope { schema_version, oneof op {Put|Delete|Txn|Lease} }`、`Entry`、`Event`、`Status`。
2. FSM.Apply 改 `proto.Unmarshal`；leader marshal 时写 magic byte `0x02`，兼容期 `0x01`/无头为旧 JSON。
3. 运行 `scripts/compact-and-snapshot.sh` 触发全量 snapshot（pb 格式），log truncate 到 last snapshot index——**杜绝混合 log 持久化**。

**收益**：

- Decode 成本 ~60% 降；log 体积 ~40% ↓。
- 为 Lease / Txn / 多字段扩展提供前提。

**成本**：

- 引入 `protoc` + Go plugin；CI 多一步。
- 一次不可逆 compaction 剧本，预发演练后再生产跑。
- 兼容期 `FSM.Apply` 多 ~30 行 magic-byte dispatch。

---

### 3.2 Theme B — 写路径性能：55 → 5 000 RPS

**现状**：每 `Apply` 走一条 Raft entry，每条 entry 一次 bbolt TX + 一次 fsync `@raft/node.go:291-316`。

**问题**：NVMe fsync ~100µs，APFS ~5ms；**串行 fsync 是整条链的绝对瓶颈**。压测数字直接佐证：c=16 → 55 RPS → p99=541ms。

**方案（四层叠加）**：

| 层 | 手段 | 单独增益 | 累计 |
|---|---|---:|---:|
| L1 | `raft.ApplyBatch`：客户端窗口 1–5ms 或 64 条一次 Apply | 4–8× | — |
| L2 | FSM.Apply 批量 bbolt TX：一 TX 消化一 batch | 3–5× | — |
| L3 | Group-commit fsync：`bbolt.NoSync=true` + 每 1–5ms 显式 `db.Sync()` | 2–4× | — |
| L4 | Wire 改 pb | 1.2× | **合计 30–80×** |

**为什么不 250×？** Raft log append + AppendEntries RPC 本身 ~500µs/batch，batch 再大也省不掉；真要到 10k RPS 需 FSM 层 sharded write（v2 再说）。

**伪接口**：

```go
func (n *Node) ApplyBatch(ctx context.Context, ops []Op) ([]*OpResult, error)
// FSM 一条 raft.Log 消化一个 batch；一 bbolt TX 一次 fsync
```

**必须同步落地的观测**：

- Histogram：`paladin_raft_apply_duration_seconds{phase="serialize|replicate|fsm|fsync"}`
- Counter：`paladin_raft_batch_size_total` (bucket by size)
- Gauge：`paladin_raft_pending_applies`

**风险**：低 QPS 下 p50 涨一个 batch 窗口（180ms→185ms）——可接受的正确取舍。

---

### 3.3 Theme C — 读一致性可选：引入 ReadIndex

**现状**：`Node.Get` 直接读本地 bolt `@raft/node.go:166-168`；**stale by apply lag**。

**问题**：配置中心常见"刚写完就读"在 feature flag / 灰度百分比场景下是 correctness bug。

**方案**：新增一条线性一致读路径，客户端显式 `?consistent=true` 才走。

```text
ReadIndexGet(key):
  1. raft.Barrier()   // 或 VerifyLeader()
  2. 记录 committedIndex=C
  3. 等 appliedIndex ≥ C（or ctx timeout）
  4. 本地 bolt.Get
```

**接口**：

```go
func (n *Node) ReadIndexGet(ctx context.Context, key string) (*store.Entry, error)
func (n *Node) ReadIndexList(ctx context.Context, prefix string) ([]*store.Entry, error)
```

**收益**：关键读路径 = etcd linearizable read 语义；写侧零成本。

**成本**：ReadIndex 多一次 heartbeat 往返（LAN ~200µs，跨 AZ ~2ms）。默认仍 stale，显式 opt-in。本次用 **Barrier 模拟**，避免对 `hashicorp/raft` 强版本依赖。

---

### 3.4 Theme D — Watch 流化与索引化

**现状**：

- HTTP long-poll，30s 窗口，每次 O(N) 扫环形缓冲 `@store/watch.go:139-150`。
- 每次 `WaitForEvents` 分配 goroutine + `time.NewTimer` `@store/watch.go:105-120`。
- 慢 watcher 静默驱逐 `@store/watch.go:62-72`。

**问题**：

- 10k 并发 SDK 轮询建连/销毁的 ~200µs overhead，累积 leader 每秒 ~30k 次 TCP 半握手。
- ring buffer N=4096 每次扫 ~50µs；事件风暴下锁争用让 goroutine 排队到秒级。
- 静默驱逐是**语义正确性问题**：客户端以为没事件，其实丢了。

**方案三步**：

**D-1. 协议 HTTP long-poll → gRPC server-streaming**

```proto
rpc Watch(stream WatchRequest) returns (stream WatchResponse);

message WatchResponse {
  int64 watch_id = 1;
  bool  compacted = 2;        // <-- 显式错误，不再静默
  int64 compact_revision = 3;
  repeated Event events = 4;
  bool  created = 5;
  bool  canceled = 6;
}
```

HTTP 网关继续支持 `/api/v1/watch/...` 作为 legacy，但新 SDK 走 gRPC。

**D-2. Ring buffer → 分段索引**

- 按 revision **有序追加** → `revision > afterRev` 由 O(N) 降到 O(log N + M)。
- 按 prefix 的 radix tree 索引 → "prefix=xxx" 过滤同为 O(log N + M)。

**D-3. 显式 `compacted` 错误**

当 `afterRev < oldestRetainedRev` → 返回 `compacted=true`；客户端必须 `FullSync` 重拉。

**收益**：单 leader watcher 承载 1k → **10k+**；E2E 通知从最坏 30s → ≤20ms；watch 语义正确性达成。

**成本**：引入 gRPC server（见主题 F）；long-poll 保留 ≥6 个月兼容期。

---

### 3.5 Theme E — API 扩展：Lease / Txn / CAS

这三样是"配置中心 → 通用协调服务"的分水岭。有了它们，PaladinCore 可承担：leader election、feature flag with TTL、分布式锁、schema migration。

**E-1. Lease（带 TTL 的 key）**

```proto
service Lease {
  rpc Grant(LeaseGrantRequest) returns (LeaseGrantResponse);
  rpc Revoke(LeaseRevokeRequest) returns (LeaseRevokeResponse);
  rpc KeepAlive(stream KeepAliveRequest) returns (stream KeepAliveResponse);
}
```

`PutWithLease(key, value, lease_id)`：lease 过期（未 KeepAlive）则 key 自动删。

实现要点：

- Lease 元数据存 `__paladin/lease/{id}` 前缀，走 Raft 复制。
- **Leader 本地维护到期 heap**；到期触发 `RevokeOp` 走 Raft。
- KeepAlive 是 gRPC bi-stream；leader 内存刷新，降频（5s 一次）持久化，避免每次都打 Raft。
- **切主后新 leader 必须从持久状态重建到期索引**——最容易出 bug 的点，混沌必测。

**E-2. Txn（If/Then/Else，CAS 是其子集）**

```proto
message TxnRequest {
  repeated Compare compare = 1;  // mod_rev / create_rev / version / value
  repeated Op      success = 2;
  repeated Op      failure = 3;
}
```

**在 FSM.Apply 内判定 compare**，保证判定与执行原子。CAS = `If{mod_rev==X} Then{Put(k,v)}`。

**E-3. Compaction 与 Defrag**

- `/admin/compact?revision=X`：丢弃 rev ≤ X 历史事件（不影响当前值，影响 watcher 起点）。
- `/admin/defrag`：在线 bbolt `Compact()` 回收空洞。

**收益**：可覆盖 ~80% etcd v3 场景；SDK 可提供 `Session` 抽象大幅简化业务代码。

**成本**：Lease 切主重建逻辑复杂；Txn 冲突回放语义需认真设计。

---

### 3.6 Theme F — 多协议 API：gRPC 主 + HTTP 网关

**现状**：只有 HTTP/JSON `@server/server.go`。

**问题**：

- Watch 热点但 HTTP/1.1 无法高效 server push。
- HTTP/1.1 每请求独占 TCP，一个 SDK 开 3 个长轮询吃 3 连接。
- 无 schema 导致 SDK 与服务端契约漂移。

**方案**：

- **gRPC 为主**：`paladinpb.v1.KV`、`Watch`、`Lease`、`Cluster`、`Maintenance`。
- **HTTP 走 `grpc-gateway`**：现有 `/api/v1/config/...` URL 保留。
- **H2 多路复用**：一条 SDK 连接同时跑 Get/Watch/KeepAlive。

**端口规划**：

- `:2379` gRPC（借 etcd 惯例）
- `:2380` Raft peer
- `:2381` HTTP (gateway + admin)
- `:2382` pprof/metrics（内网 + auth）

**收益**：10k SDK 连接数降一个数量级；类型安全 wire。

**成本**：gRPC 依赖（已在链上）；grpc-gateway codegen 一次性投入。

---

### 3.7 Theme G — SDK 韧性升级

**现状**：SDK 只用 `Addrs[0]`；其他地址仅用于缓存文件命名。长轮询失败后 constant backoff，不切 endpoint。

**问题**：leader HTTP 宕时直接进入退避循环，拿不到新 leader，事实上变成**单点**。

**方案**：

**G-1. Endpoint Pool + Health-Aware**

```go
type EndpointPool struct {
    endpoints []Endpoint
    healthy   atomic.Pointer[[]int]      // 索引
    weights   map[int]*atomic.Int64      // EWMA p95 latency
}
func (p *EndpointPool) Pick() Endpoint
```

后台 health loop 每 2s 探测；失败三次 unhealthy；EWMA α=0.2 延迟加权，慢节点自然减权。

**G-2. 请求类型感知路由**

- **写**：gRPC `KV.Put` → `LEADER_UNAVAILABLE` → 拉 `MemberList` → 重试 ≤3 次指数退避。
- **Stale 读**：任意健康 endpoint。
- **Consistent 读**：只发 leader；leader 切换中短暂 backoff。
- **Watch/Lease**：粘住一个 endpoint；断开重选。

**G-3. Hedging（可选）**

p99 敏感读：20ms 后对冲第二个 endpoint，先到者赢。默认关闭。

**G-4. 本地缓存升级**

- schema version；不匹配拒绝使用。
- 增量持久化：watch 事件即刻更新本地副本；启动从缓存回放 `last_revision` 再 watch。
- 损坏降级：保留 `.bak`，新缓存写失败回滚。

**收益**：SDK **单节点宕机无感**；leader 切换期间读无中断；开 hedging 后 p99 读延迟再降 ~40%。

**成本**：SDK 代码量 +~400 行；需专门集成测试。

---

### 3.8 Theme H — 稳定错误协议

**现状**：`@server/raft_server.go:122` `err.Error() == "apply error: key not found"` 字符串比较。任何 errorf 文案改动都让 delete 的 404 逻辑退化为 500。

**方案**：类 gRPC status codes + 错误 details。

```proto
enum Code {
  OK = 0;
  KEY_NOT_FOUND = 1;
  PRECONDITION_FAILED = 2;  // Txn compare 未过
  LEADER_UNAVAILABLE = 3;
  COMPACTED = 4;
  QUOTA_EXCEEDED = 5;
  PERMISSION_DENIED = 6;
  UNAUTHENTICATED = 7;
  INVALID_ARGUMENT = 8;
  INTERNAL = 99;
}
```

HTTP gateway 映射：

| Code | HTTP |
|---|---:|
| OK | 200 |
| KEY_NOT_FOUND | 404 |
| PRECONDITION_FAILED | 412 |
| LEADER_UNAVAILABLE | 503 |
| COMPACTED | 410 |
| QUOTA_EXCEEDED | 429 |
| PERMISSION_DENIED | 403 |
| UNAUTHENTICATED | 401 |
| INVALID_ARGUMENT | 400 |
| INTERNAL | 500 |

所有错误走 `status.Error(codes.X, ...)`；CI lint 禁用 `err.Error() ==`。完整码表见附录 B。

---

## 4. 生产能力补强

### 4.1 安全

**TLS**：

- 所有监听端口默认 TLS；自签开发证书内置，生产必须替换。
- `--tls-cert/--tls-key/--tls-ca` 或从 `paladin.yaml` 读。
- Peer 间（Raft transport）独立 mTLS；证书 rotation 走 cert-manager 或 Vault PKI。

**认证**：

- gRPC metadata / HTTP header：`authorization: Bearer <jwt>` 或 `Basic ...`。
- 多 provider：内置 static users、JWT (HS256/RS256)、可选 OIDC（P5 之后）。

**授权（RBAC）**：

- 对象：`User / Role / RoleBinding`。
- 最小粒度：`(verb, path_prefix)`；verb ∈ {read, write, watch, admin}。
- 默认角色：`root`、`read-only`、`tenant-admin`（限 prefix）。
- ACL 数据自身走 Raft 复制，存 `__paladin/acl/` 前缀。

**审计日志**：

- 每次写 + 每次 admin → 结构化 audit event，append-only 文件 + 可选 SIEM。
- 字段：`ts, actor, src_ip, verb, path, before_rev, after_rev, status, trace_id`。

### 4.2 多租户 & 配额

- **Quota 维度**：`max_keys / max_bytes / max_qps_write / max_qps_read / max_watchers`。
- **实现**：leader 维护按租户 counter；超额 `QUOTA_EXCEEDED`。
- **速率限制**：每租户 token bucket（`golang.org/x/time/rate`）在 gRPC interceptor。
- **隔离等级**：v1 软隔离（共享 Raft group）；v2 可选硬隔离（per-tenant Raft group，本次不做）。

### 4.3 可观测性（metrics / logs / traces）

**Metrics（Prometheus）**：RED + USE + 业务三类。

| 类别 | 指标 | 类型 | 标签 |
|---|---|---|---|
| RED | `paladin_rpc_duration_seconds` | Histogram | service, method, code |
| RED | `paladin_rpc_requests_total` | Counter | service, method, code |
| RED | `paladin_rpc_errors_total` | Counter | service, method, code |
| USE | `paladin_raft_pending_applies` | Gauge | — |
| USE | `paladin_raft_leader_changes_total` | Counter | — |
| USE | `paladin_bolt_tx_duration_seconds` | Histogram | type |
| USE | `paladin_fsync_duration_seconds` | Histogram | — |
| USE | `paladin_goroutines` | Gauge | — |
| Biz | `paladin_keys_total` | Gauge | tenant |
| Biz | `paladin_watch_streams_active` | Gauge | tenant |
| Biz | `paladin_lease_active` | Gauge | tenant |
| Biz | `paladin_revision` | Gauge | — |

**Logs（`log/slog`）**：JSON 格式，字段 `ts, level, msg, service, method, trace_id, span_id, tenant, actor, key?, err?`。禁用 stdlib `log`；CI lint。

**Traces（OpenTelemetry）**：HTTP/gRPC interceptor 承继 `traceparent`；关键 span：`rpc.server / raft.apply / bolt.tx / watch.dispatch / sdk.full_pull`。OTLP exporter 指向 collector。

**Health**：

- `/healthz`（liveness）：进程未死、FSM 已启动。
- `/readyz`（readiness）：Raft 已发现 leader、apply lag < 10s、bolt 可写。

### 4.4 运维

**单一配置文件** `paladin.yaml`：

```yaml
node:
  id: node1
  data_dir: /var/lib/paladin
cluster:
  initial_members:
    - id: node1
      peer_addr: 10.0.0.1:2380
      client_addr: 10.0.0.1:2379
listen:
  grpc:  0.0.0.0:2379
  peer:  0.0.0.0:2380
  http:  0.0.0.0:2381
  pprof: 127.0.0.1:2382
tls:
  client_cert: /etc/paladin/client.crt
  client_key:  /etc/paladin/client.key
  peer_cert:   /etc/paladin/peer.crt
  peer_key:    /etc/paladin/peer.key
  client_ca:   /etc/paladin/ca.crt
raft:
  heartbeat_timeout: 500ms
  election_timeout: 1500ms
  snapshot_threshold: 100000
  apply_batch_size: 256
  apply_batch_window: 2ms
auth:
  mode: token    # none | token | jwt | oidc
observability:
  metrics_addr: 127.0.0.1:2382
  otlp_endpoint: otel-collector:4317
  log_format: json
  log_level: info
```

**热更新**：`SIGHUP` 或 `/admin/reload`，仅允许 observability/auth/log_level/quota；其他须重启。

**Backup**：

```bash
paladin-core backup --endpoint=127.0.0.1:2379 --out=paladin-20260420.snap
```

实现：gRPC `Maintenance.Snapshot`；leader 用 bbolt `db.View(tx.WriteTo)` 一致性快照流式导出 + 校验和。

**Restore**（冷启动）：

```bash
paladin-core restore --snap=paladin-20260420.snap --data-dir=/var/lib/paladin-new --initial-cluster=...
```

**滚动升级剧本**：

1. 确认集群健康、`raft_pending_applies < 10`。
2. **Follower → Leader** 顺序，一次一个。
3. 升级前 `kill -TERM`（优雅停机：drain watch → Barrier → shutdown）。
4. 新版本启动等 `readyz=true`。
5. 观察 10 min metrics；无异常再下一个。

**Graceful shutdown 要点**：

- SIGTERM → 停接新连接 → watch stream 发 `canceled=true` → 在飞 txn 等完（或 30s 超时）→ Raft Barrier → Shutdown。
- 自己是 leader 时 → 主动 `MoveLeader` 到健康 follower，避开 election 窗口。

### 4.5 SRE 工具

- `/debug/pprof/*`：内网专用，token-gated。
- `/admin/compact?revision=X`、`/admin/defrag`、`/admin/move-leader?to=NODE`。
- `paladin-core inspect` CLI：离线读 bbolt，列 keys / 统计体积 / 查 raft-log.db index range——**事故现场必备**。

---

## 5. 性能优化路线图

### 5.1 写路径 p99 预算

"我们只有 50ms 的 p99 预算，每个环节分到多少？"

| 环节 | 基线 | 目标 | 关键措施 |
|---|---:|---:|---|
| gRPC parse + auth | 未测 | 1ms | pb decode + 常驻 role cache |
| Leader queue wait | ~40ms | 10ms | ApplyBatch 削峰 |
| Raft replicate (RTT) | ~1ms | 5ms | 保持（LAN） |
| FSM apply (decode + bolt tx) | ~100ms 串行 | 15ms | 批量 TX |
| fsync | ~40ms | 15ms | group commit |
| Response marshal + return | 未测 | 4ms | pb + gzip off |
| **合计 p99** | **541ms** | **≤50ms** | — |

### 5.2 读路径预算

Stale p99 ≤ 5ms，Consistent p99 ≤ 15ms。

| 环节 | Stale 预算 | Consistent 预算 |
|---|---:|---:|
| gRPC parse + auth | 0.5ms | 0.5ms |
| ReadIndex barrier | — | 5ms |
| BoltDB read | 1ms | 1ms |
| Marshal + return | 0.5ms | 0.5ms |
| Server-side | **2ms** | **7ms** |
| LAN RTT | 2ms | 2ms |
| **合计** | **≤5ms** | **≤15ms** |

### 5.3 GC / 内存压力

按 ROI 排序的优化点：

1. **`sync.Pool` 化 request context / proto buffer / WatchResponse**——一条 pb 响应 allocs 从 12 降到 2。
2. **bbolt Get 零拷贝**：`Get` 返回 tx 内 slice，外层显式 copy；调用方须在 tx 结束前消费。
3. **长轮询 / stream header map 复用**。
4. **Event slice 预分配 cap 64**，避免窗口内 append 扩容。

### 5.4 具体 Perf TODO（按 ROI 排序）

| # | 动作 | 预期收益 | 工作量 |
|---|---|---:|---:|
| 1 | `raft.ApplyBatch` + FSM batch bolt TX | 写 RPS 5–8× | M |
| 2 | group fsync (NoSync + periodic Sync) | 写 RPS 2–4× | M |
| 3 | pb wire | 写/读 1.2×，内存 ~30%↓ | M |
| 4 | WatchCache 二分索引 + prefix radix | watch p99 降 2× | M |
| 5 | gRPC H2 多路复用替 long-poll | 连接数 10×↓ | L |
| 6 | sync.Pool 化 hot objects | GC STW 降 30% | S |
| 7 | bbolt 零拷贝 Get | 读 CPU 降 15% | M |
| 8 | Snapshot 流式持久化（chunked `tx.WriteTo`） | 重启时间 10×↓ | M |
| 9 | Raft heartbeat/election tuning | failover 窗口 0.8s | S |

---

## 6. 分阶段落地计划

每阶段都有明确 **entry / exit 标准**；exit 不达标不进下一阶段。

### Phase 0 — 地基（2–3 周）

**Entry**：当前 bench 基线已记录在 `bench/baselines/v0.1/`。

**内容**：

- `api/paladinpb/v1/` + protoc 工具链；CI 加 codegen check。
- `log/slog` 全量替换 `log.Printf`。
- `prometheus/client_golang` + `/metrics` endpoint；至少 5 个 RED 指标。
- `paladin.yaml` 配置 + env 覆盖；CLI flag 仅作 YAML sugar。
- 错误码枚举 + gRPC status 包；CI lint 禁用 `err.Error() ==` 字符串比较。
- OTLP SDK 注入；HTTP interceptor 至少有 `rpc.server` span。
- **Linter 套件接入**（§2.3.H）：`.golangci.yml` + `gofumpt` + `staticcheck` + `goleak` + `govulncheck` + `buf lint`；`make lint` 一键；PR 不绿禁 merge。

**Exit**：

- `go test -race ./...` 全绿；新增 observability smoke test。
- `/metrics` 可被 Prometheus 抓取；Grafana dashboard 示例提交 `docs/dashboards/`。
- wire 未变，bench 数字 **±5% 无回归**。

### Phase 1 — 写路径性能（3–4 周）

**Entry**：Phase 0 通过。

**内容**：

- `raft.Op` 接入 pb envelope（magic-byte dispatch 兼容 JSON）。
- `ApplyBatch` + 批量 bolt TX + group fsync。
- Snapshot 改流式（chunked `tx.WriteTo`）；Restore 批量 Put。
- 10 GB 冷启动恢复端到端 ≤ 60s。

**Exit**：

- 写 RPS @ p99 ≤ 50ms ≥ **5 000**（3-node LAN）。
- `bench-suite.sh` 新基线入 `bench/baselines/v0.2/`。
- 既有语义测试全绿；无新错误码。

### Phase 2 — 读 / Watch / SDK（2–3 周）

**Entry**：Phase 1 通过。

**内容**：

- `ReadIndex` 线性读（Barrier 模拟）；gRPC `Range(consistent=true)`。
- Watch → gRPC bi-stream；`compacted` 显式暴露；revision 二分 + prefix radix。
- SDK endpoint pool + 健康感知；watch 收 `compacted` → 自动 fullPull。
- grpc-gateway 接 HTTP 兼容层；老路径进入 deprecated 但可用。

**Exit**：

- Stale read p99 ≤ 5ms @ 500k RPS；consistent read p99 ≤ 15ms @ 50k RPS。
- 10k 并发 gRPC watch stream 稳态运行 1 小时，内存 < 2 GB。
- SDK 单节点故障恢复 ≤ 3s（混沌验证）。

### Phase 3 — API 扩展（3 周）

**Entry**：Phase 2 通过。

**内容**：

- Lease：`Grant / Revoke / KeepAlive` + PutWithLease；leader 切换重建到期索引。
- Txn：`Compare / Then / Else`；CAS 是其子集。
- `/admin/compact`、`/admin/defrag`。

**Exit**：

- Lease E2E 测试：10k leases、1k KeepAlive 并发、切主场景零 key 丢失。
- Txn race 测试：1k 并发 CAS 收敛；compare 失败明确返回 `PRECONDITION_FAILED`。
- Compact / Defrag 在线运行不中断读写。

### Phase 4 — 安全与运维（3 周）

**Entry**：Phase 3 通过。

**内容**：

- TLS / mTLS 全链路；证书 rotation 剧本。
- RBAC：User/Role/RoleBinding；默认角色内置；审计日志。
- Backup / Restore CLI。
- 滚动升级剧本 + `MoveLeader` API。

**Exit**：

- 外部渗透测试：无 auth / 弱 auth / 越权 三大类无高危。
- 10 GB 数据 backup+restore 端到端 ≤ 10 min。
- 滚动升级演练：客户端无感，p99 抖动 < 10ms。

### Phase 5 — 稳定化与发布（2 周）

**Entry**：Phase 4 通过。

**内容**：

- 混沌剧本清单（见 §8.3）；每个至少通过 3 次。
- 7×24 长跑：3-node 预发环境跑 14 天，SLO 达标。
- 灰度：内部非关键场景先上；观测 2 周后逐步放量。
- 文档：运维手册、故障排查 runbook、API 参考。

**Exit**：

- v1.0 tag；release notes；迁移文档。

---

## 7. 风险矩阵与缓解

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| **Wire 跃迁引入数据损坏** | 中 | 致命 | 预发环境先跑 compaction + snapshot 全量演练；回滚剧本预先写好，包含"从 P0 snapshot 冷恢复" |
| **ApplyBatch 引起 p50 上涨** | 高 | 中 | 可配置 batch window；SLO 违规时降级到 batch=1 |
| **Group fsync 导致异常断电丢数据** | 低 | 高 | 每个 batch 完成后才 ACK 给客户端；fsync 间隔 ≤ 5ms；断电后 Raft log 仍是 truth source |
| **Lease 切主导致 key 泄漏** | 中 | 中 | 切主后 leader 重建到期 heap 前冻结所有 lease 操作；混沌必测 |
| **Watch 流 goroutine 泄漏** | 中 | 中 | 每 stream 携带 context；shutdown 时 30s 强制 cancel；Gauge `watch_streams_active` 监控 |
| **gRPC 升级破坏老客户端** | 中 | 中 | HTTP gateway 保留 ≥ 6 月；版本号 header 强制标记；旧 SDK 用 deprecated feature 时输出 warning |
| **TLS 证书过期集群宕机** | 中 | 致命 | 证书有效期短（30d）；cert-manager 自动轮换；过期前 7 天告警 |
| **RBAC 配置错误锁死** | 中 | 高 | 保留 "root token" 紧急通道；审计日志永不禁写 |
| **性能改动引入新的锁死路径** | 中 | 高 | `-race` 常绿；goroutine 泄漏检测（`goleak`）进 test；pprof goroutine profile 入 dashboard |
| **bbolt defrag 期间性能骤降** | 低 | 中 | defrag 仅在 leader 运行且 QPS 低谷期触发；限速 |

---

## 8. 测试与验证策略

### 8.1 五层测试金字塔

| 层 | 工具 | 门槛 |
|---|---|---|
| 单元测试 | `go test -race` | 包覆盖 ≥ 80%；新代码 ≥ 90% |
| 集成测试 | 3-node in-process cluster | 每次 CI 必跑；<10 min |
| 契约测试 | protobuf + 老 client 回放 | 每次 wire 改动必跑 |
| 压测 | `@bench/`（已就位） | 每个 PR 自动对比基线；退化 > 5% 需审批 |
| 混沌 | 自研 `paladin-chaos`（或借 toxiproxy） | P5 阶段全部通过 |

### 8.2 回归基线门槛

| 指标 | 允许退化 |
|---|---:|
| 写 RPS @ p99 | ≤ 3% |
| 读 RPS | ≤ 5% |
| 写 p99 绝对值 | ≤ 5ms |
| 读 p99 绝对值 | ≤ 1ms |
| 内存稳态 | ≤ 10% |
| goroutine 稳态 | ≤ 1 000 |

超门槛 = 自动 block merge，需架构小组签字。

### 8.3 混沌剧本清单（P5 必过）

| # | 剧本 | 期望 |
|---|---|---|
| C1 | Kill leader | 800ms 内新 leader；客户端重试成功 |
| C2 | Kill follower | 读无感；leader 日志复制自动 catch-up |
| C3 | 网络分区（leader 孤立） | leader 降级；多数派侧选出新 leader |
| C4 | 磁盘满 | 写返回 `QUOTA_EXCEEDED` / `INTERNAL`，不崩 |
| C5 | 磁盘 I/O 延迟注入（100ms fsync） | 写 p99 退化但有界，不雪崩 |
| C6 | Clock skew ±5 min | Lease 到期行为与时钟同源，不提前/延后 |
| C7 | 网络抖动（5% 丢包） | Raft 心跳不 flap；failover 次数可接受 |
| C8 | SDK 侧 kill -9 | 服务端 watch stream 30s 内清理 |
| C9 | 冷启动 10 GB 数据 | ≤ 60s readyz |
| C10 | 滚动升级（3 节点） | 客户端无错；p99 抖动 < 10ms |
| C11 | Backup → Restore 循环 | 数据 SHA256 一致 |
| C12 | 同时 kill 2 个 follower（少数派侧） | 集群继续工作；只是无冗余 |
| C13 | Lease 风暴（10k 并发 KeepAlive） | 不过载、不误杀 |
| C14 | Watch 风暴（10k 连接 + 1k QPS 写） | watch p99 ≤ 200ms |

---

## 9. 附录

### 附录 A. Proto schema 草案（核心部分）

> 完整 schema 在 P0 阶段落入 `api/paladinpb/v1/*.proto`；此处只列关键形状以便讨论。

```proto
syntax = "proto3";
package paladin.v1;

message Entry {
  string key             = 1;
  bytes  value           = 2;
  int64  create_revision = 3;
  int64  mod_revision    = 4;
  int64  version         = 5;
  int64  lease_id        = 6;   // 0 = 无 lease
}

message Event {
  enum Type { PUT = 0; DELETE = 1; }
  Type   type       = 1;
  Entry  entry      = 2;
  Entry  prev_entry = 3;
}

// Raft log payload：磁盘格式 = [magic_byte(0x02)] + proto.Marshal(OpEnvelope)
message OpEnvelope {
  uint32 schema_version = 1;     // 当前 2
  string request_id     = 2;     // 幂等 key
  oneof op {
    PutOp    put    = 10;
    DeleteOp delete = 11;
    TxnOp    txn    = 12;
    LeaseOp  lease  = 13;
  }
}

message PutOp    { string key = 1; bytes value = 2; int64 lease_id = 3; }
message DeleteOp { string key = 1; string range_end = 2; }

message Compare {
  enum Target { VALUE = 0; CREATE = 1; MOD = 2; VERSION = 3; }
  enum Result { EQUAL = 0; GREATER = 1; LESS = 2; NOT_EQUAL = 3; }
  Target target = 1;
  Result result = 2;
  string key    = 3;
  oneof target_union {
    bytes  value           = 4;
    int64  create_revision = 5;
    int64  mod_revision    = 6;
    int64  version         = 7;
  }
}

message TxnOp {
  repeated Compare compare = 1;
  repeated OpEnvelope success = 2;
  repeated OpEnvelope failure = 3;
}

message LeaseOp {
  oneof op {
    LeaseGrant  grant  = 1;
    LeaseRevoke revoke = 2;
  }
}

message Status {
  Code     code    = 1;
  string   message = 2;
  repeated google.protobuf.Any details = 3;
}

enum Code {
  OK                  = 0;
  KEY_NOT_FOUND       = 1;
  PRECONDITION_FAILED = 2;
  LEADER_UNAVAILABLE  = 3;
  COMPACTED           = 4;
  QUOTA_EXCEEDED      = 5;
  PERMISSION_DENIED   = 6;
  UNAUTHENTICATED     = 7;
  INVALID_ARGUMENT    = 8;
  INTERNAL            = 99;
}
```

### 附录 B. 完整错误码表

| Code | gRPC | HTTP | 场景 | 客户端动作 |
|---|---|---:|---|---|
| `OK` | OK | 200 | 成功 | — |
| `KEY_NOT_FOUND` | NOT_FOUND | 404 | Get/Delete 不存在的 key | 对齐业务语义，可忽略 |
| `PRECONDITION_FAILED` | FAILED_PRECONDITION | 412 | Txn compare 未通过 | 回读当前值重试 |
| `LEADER_UNAVAILABLE` | UNAVAILABLE | 503 | Leader 宕/切换中 | 指数退避重试，重选 endpoint |
| `COMPACTED` | OUT_OF_RANGE | 410 | Watch after_rev 已被 compact | 触发 FullSync，从最新 revision 重开 watch |
| `QUOTA_EXCEEDED` | RESOURCE_EXHAUSTED | 429 | 租户超额 | 上游限流，不可无脑重试 |
| `PERMISSION_DENIED` | PERMISSION_DENIED | 403 | RBAC 拒绝 | 不重试，报给用户 |
| `UNAUTHENTICATED` | UNAUTHENTICATED | 401 | 无/错 token | 不重试，刷新凭据 |
| `INVALID_ARGUMENT` | INVALID_ARGUMENT | 400 | 入参非法 | 修复代码，不重试 |
| `INTERNAL` | INTERNAL | 500 | 未分类错误 | 退避重试，记日志报警 |

### 附录 C. 兼容性迁移 Checklist

**wire 跃迁前（v0.x → v1.0）**：

- [ ] 预发环境跑完整 compaction + snapshot，数据校验 SHA256。
- [ ] 回滚剧本：从 P0 snapshot 冷启动路径可用。
- [ ] 老 SDK 对 HTTP 路径的回归测试全绿。
- [ ] 文档更新 CHANGELOG + UPGRADE.md。

**滚动升级前（每次 minor）**：

- [ ] `raft_pending_applies < 10`、无 leader 变更 10 min+。
- [ ] 备份导出成功、SHA256 一致。
- [ ] Canary 节点跑 1h 无异常。
- [ ] p99 观测窗口 ±5% 无异常。

**每次发布**：

- [ ] `bench-suite.sh` 全绿、无基线回归。
- [ ] 所有混沌剧本通过。
- [ ] 公开 API 无破坏性变更（或写明废弃窗口）。
- [ ] CHANGELOG 标 `BREAKING:` 字段。

### 附录 D. 反路线图（明确 *不* 做的事）

> 工程的质感有一半来自"克制"。以下项目对本次重构 scope 永久关闭；若有人提议，请先证明它们同时满足：（a）有真实需求；（b）能在 SLO 之下实现；（c）团队容量 > 项目现状。

- ❌ **多 DC 复制**——单 Raft group 的延迟和一致性够用；跨 region 要的是另一套系统（e.g. etcd Learner + 最终一致）。
- ❌ **分片 / 水平扩展写**——10M keys 以内单 group 能撑；再往上建议换 TiKV。
- ❌ **Serverless 部署**——Raft 需要稳定成员身份，和 FaaS 模型冲突。
- ❌ **嵌入式模式（as library）**——会暴露 Raft 内部，兼容性责任爆炸。
- ❌ **自定义存储引擎**——bbolt 的"单写多读 + mmap"特性已是 Raft FSM 的最优解。
- ❌ **Web UI / 图形 admin 面板**——`curl` + `grafana` 已足够；做 UI 会分散重构带宽。
- ❌ **Paxos / Multi-Raft**——单 group 的简单性就是价值本身。

---

## 结语

这份蓝图的核心不是"发明"，而是**把已知的正确答案按成本降序排好，依次执行**：

1. 先补地基（P0）让所有人讲同一种话——proto、slog、metrics、错误码。
2. 再治**最确定**的瓶颈（P1 写路径）——从压测数字看，这里 1 的投入换 50 的回报。
3. 然后把读侧和 watch 真正做成生产级（P2）。
4. 在稳固的架构上叠加业务能力（P3 Lease/Txn）。
5. 最后补齐安全与运维（P4）——这些功能本身不难，难的是做在已经稳定的底座上。
6. 用混沌和长跑证明给自己看（P5）。

**不变的是五层架构、Revision 语义、SDK 三段式**；**改变的是每一层的细节实现与协议契约**。

> 生产级不是"多写很多代码"，是"把已经写的每一行代码重新审视一遍，问它能不能在凌晨 3 点的 oncall 里依然自证清白"。
