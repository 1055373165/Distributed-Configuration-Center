# PaladinCore v1.0 生产化 PRD

> **本文档定位**：从用户与产品角度定义 v1.0 要"做什么、给谁用、怎么算成功"。
> **配套工程蓝图**：`docs/production-refactoring.md`（八大主题、性能预算、phase 拆分、proto schema 草案）。
> **本 PRD 是 WHAT / WHY / WHO，蓝图是 HOW / WHEN。**两份互引、不重复。
>
> 关系图：
> ```
> 用户问题  ─┐
>           ├──▶ 本 PRD（验收标准） ──▶ production-refactoring.md（实现路径）──▶ Sprint 看板
> 业务目标  ─┘
> ```

---

## 0. 文档信息

| 字段 | 内容 |
|---|---|
| 版本 | v1.0-draft-1 |
| 状态 | DRAFT — 等待架构 / SRE / 安全 sign-off |
| 范围 | PaladinCore v0.x（教学/alpha）→ v1.0（小型生产配置中心） |
| 关联文档 | `docs/production-refactoring.md`（蓝图）、`docs/paladincore-v2-spec.md`（v2 范围）、`TODOS.md`（OV 开放问题）、`README.md`（现状） |
| Out of scope | 多 DC / 跨 region 复制、水平扩写（multi-raft sharding）、Web UI、Serverless 部署（详见 §10 与蓝图附录 D） |
| 假设若不成立则本 PRD 需局部重写 | (1) 单集群 ≤ 50 GB / 10M keys / 10k SDK；(2) 允许一次 wire 跃迁 (JSON→pb)；(3) 单 Raft group；(4) 生产 NVMe + Linux |

---

## 1. 背景与问题陈述

### 1.1 现状一句话

PaladinCore 是一个 ~2000 行的 Raft 配置中心**教学艺术品**——架构正确、语义对齐 etcd v3、可在三节点跑通。但它在 README 里自我定位为 "alpha / educational"，**不能上生产**。

### 1.2 核心矛盾

我们已经写出了一个"对的"分布式系统的最小内核：**五层窄接口、Revision 语义、Watch ring buffer、Leader 透明转发、SDK 三段式生命周期**。这些抽象都站得住。**问题不是设计错了，是没做完。** v1.0 要做的不是重新设计，是把"对的设计"补到生产可用的细节密度。

### 1.3 现状不能上生产的五条硬阻塞（按用户视角）

| # | 用户感受 | 工程实质 | 锚点 |
|---|---|---|---|
| B1 | "我的写请求 p99 540ms，平时还能用，发布期 c=16 直接超时" | 单 fsync 串行；无 ApplyBatch | `raft/node.go:147-174` |
| B2 | "Watch 突然没事件了，但我也不知道是没变还是丢了" | Ring buffer 静默淘汰，无 `compacted` 错误 | `store/watch.go:126-151` |
| B3 | "我没法把它放进 VPC——没 TLS、没鉴权、谁都能写" | 完全裸协议 | `server/server.go` 全局 |
| B4 | "我没法升级集群——一升级客户端就报错" | 错误协议靠字符串比较；无版本 | `server/raft_server.go:137`、`raft/node.go:34` |
| B5 | "我没法运维——出问题不知道哪里崩，没指标没 trace 没 backup" | 可观测性栈未建；无 backup/restore 工具 | 全局 |

### 1.4 v1.0 要回答的根本问题

> **"我能不能把 PaladinCore 放进我们公司一个真实业务的 VPC，让 50 个微服务去用它做配置和发布开关，不需要 oncall 每周三次救火？"**

这就是 GA 的客观判据。本 PRD 的所有验收标准都服务于这个问题。

---

## 2. 目标用户与使用场景

### 2.1 主要用户（Personas）

#### P1. 平台 SRE（决策者 + 主用户）

- 团队规模 5-20 人，负责公司基础设施
- 已有 etcd / Consul / Apollo 之一在线运行；评估 PaladinCore 是因为想要"小到能读懂、改得动"的方案
- **最关心**：可观测性、滚动升级、故障可定位、跑得稳
- **不在乎**：跑分领先一线产品、特性最多
- **痛点**：现有 etcd 集群成了黑盒，团队没人能改动；想换个能掌控的小方案

#### P2. 微服务后端工程师（高频用户）

- 通过 SDK 接入，每天几十次读、几次写
- **最关心**：SDK 不要在节点宕机时阻塞业务、配置变更要秒级生效、本地缓存能撑过服务端重启
- **不在乎**：底层用什么共识算法
- **痛点**：从 Apollo 过来的人会习惯性问"灰度发布、环境分组、UI 在哪"——v1.0 要诚实告诉他们答案是"不在 v1.0"

#### P3. 发布平台/灰度系统集成方（关键场景）

- 把 PaladinCore 当 feature flag 后端、灰度百分比存储、发布开关
- **最关心**：写完立刻能读到（线性一致读）、Watch 不丢事件、CAS 能正确做并发控制
- **不在乎**：数据量大（feature flag 通常 < 1MB 总量）
- **这是 v1.0 的"杀手用例"**——如果不能稳定支持发布开关，v1.0 就失败

#### P4. 教学/学习/研究（保留 persona）

- 学生、面试者、自学者读源码
- **最关心**：代码可读、文档详细、能在笔记本跑起来
- v1.0 不能因为生产化牺牲这个 persona——README 一直自夸 "read-sized"，不能因为加了 RBAC / TLS 就让代码膨胀到不可读
- **硬约束**：核心 5 个包总行数 ≤ 6000 行（来自 `paladincore-v2-spec.md` 的 north star）

### 2.2 用户场景画像（按真实负载）

| 场景 | 写 QPS | 读 QPS | Watcher 数 | 数据量 | 单值大小 |
|---|---:|---:|---:|---:|---:|
| 中小公司全公司配置中心 | 平均 1，发布期 50 | 5 000 | 500-5 000 | 100 MB | < 10 KB |
| 单业务线发布开关后端 | 0.1 | 200 | 50-500 | 10 MB | < 1 KB |
| 微服务协调（Lease/Lock） | 50（KeepAlive 流量） | 100 | 100 | < 10 MB | < 100 B |
| 教学/单机 demo | < 1 | < 100 | < 10 | < 10 MB | 任意 |

**v1.0 必须支持前三类。第四类是免费送的——只要前三类做好，第四类自然成立。**

### 2.3 显式不服务的用户

- 配置数据 > 10 GB 的（建议 etcd / TiKV）
- 需要跨 region 强一致的（建议 CockroachDB / Spanner）
- 需要图形化 admin UI 的（建议 Apollo / Nacos）
- 写 QPS > 10k 持续负载的（建议 etcd v3.5+）

明确说不，比含糊承诺更有价值。

---

## 3. 目标 / 非目标

### 3.1 业务目标（Why）

| 目标 | 度量 |
|---|---|
| 把 PaladinCore 从"教学项目"升到"小型生产可用" | v1.0 GA 后，至少 1 个真实业务在 VPC 内连续运行 90 天无 P0 事故 |
| 维持"小到能读懂"的核心心智优势 | 核心 5 个包总行数 ≤ 6000；外部贡献者第一周内能提 PR |
| 在配置中心 + 发布开关场景成为可信替代 | feature-flag 集成方反馈 SLO 达标率 ≥ 99% |

### 3.2 产品目标（What 必须做）

| ID | 目标 | 验收 |
|---|---|---|
| G1 | 写路径生产级 | 写 RPS @ p99 ≤ 50ms ≥ 5 000；mixed_95r5w p99 ≤ 30ms |
| G2 | 读路径双轨 | Stale read p99 ≤ 5ms @ 500k RPS；consistent read p99 ≤ 15ms @ 50k RPS |
| G3 | Watch 真生产级 | 10k 并发 stream 稳定 1h；E2E p99 ≤ 200ms；语义正确（不再静默丢） |
| G4 | 协调原语 | Lease / Txn (CAS) / Compact 完整可用 |
| G5 | 部署到 VPC 安全可行 | TLS / mTLS / RBAC / 审计日志全闭环 |
| G6 | 全套运维工具 | Backup / Restore / 滚动升级 / 监控大盘 / 故障 runbook |
| G7 | SDK 可用性 | 单节点宕机对客户端无感（恢复 ≤ 3s）；leader 切换读不中断 |
| G8 | 协议稳定性 | wire 一次跃迁后冻结；错误码语义化；wire 兼容性测试入 CI |

### 3.3 非目标（v1.0 明确不做）

- ❌ 多 DC / 跨 region 复制
- ❌ 水平扩写（multi-raft sharding）
- ❌ Web 管理 UI
- ❌ Serverless / 嵌入式（library mode）部署
- ❌ 自定义存储引擎（继续 bbolt）
- ❌ Apollo/Nacos 风格的"环境/集群/分组/灰度"完整产品树（这些是用户场景，由调用方在我们的原语之上拼装）
- ❌ 流式 SQL / 任何查询语言
- ❌ 自带 secret 管理（用 Vault / cert-manager）

详见蓝图 `production-refactoring.md` §9 附录 D《反路线图》。

### 3.4 v1.0 vs v2 范围分界

参考 `docs/paladincore-v2-spec.md`：v1.0 包含蓝图的 P0-P5；v2 处理多 raft group 分片实验、公开 benchmark 看板、Jepsen 集成等（已 deferred 到 TODOS.md）。

---

## 4. 成功度量（SLO + KPI）

### 4.1 性能 SLO（运行时）

| 指标 | v0.x 基线 | v1.0 目标 | 测量 |
|---|---:|---:|---|
| 写 p99 @ c=16 | 541ms | **≤ 50ms** | bench-suite write_only |
| 读 p99 @ c=16 | 550µs | **≤ 5ms @ 500k RPS** | bench-suite read_only |
| Mixed p99 @ 95r5w | 174ms | **≤ 30ms** | bench-suite mixed |
| Watch E2E p99 | ~30s（长轮询窗口） | **≤ 200ms** | 自研 watch-latency 工具 |
| Leader failover 写不可用窗口 | ~2s | **≤ 800ms** | C1 混沌剧本 |
| 10 GB 冷启动恢复 | 未实现 | **≤ 60s** | C9 混沌剧本 |
| Backup → Restore 端到端 | 未实现 | **≤ 10min @ 10GB** | 自动化测试 |

### 4.2 可用性 SLA（公开承诺）

| 维度 | 目标 | 度量窗口 |
|---|---:|---|
| 三节点集群可用性 | **99.99%** | 月度 |
| 写请求成功率（排除客户端 4xx） | **99.95%** | 月度 |
| Watch 通知不丢失率（按事件计） | **100%**（要么收到，要么显式 `compacted`） | 永远 |

### 4.3 用户体验 KPI

| 指标 | 目标 |
|---|---:|
| SDK 在单节点宕机时业务无感（无 5xx 透出业务） | 100% 场景 |
| 新用户 quickstart 时间（cluster up + 第一次写读） | ≤ 5 分钟 |
| 故障 runbook 覆盖度（已识别故障模式有对应 runbook） | ≥ 90% |
| Grafana 大盘开箱即用（导入即可看） | ≥ 1 个 |

### 4.4 项目健康 KPI

| 指标 | 目标 |
|---|---:|
| 核心 5 个包总行数（store/raft/server/sdk/cmd） | ≤ 6000 |
| 单测包覆盖率 | ≥ 80%（新代码 ≥ 90%） |
| `go test -race` 常绿率 | 100% |
| CI 时间（push → 全套 lint+test+integ） | ≤ 10 分钟 |
| 外部贡献者首 PR 通过周期 | ≤ 7 天 |

---

## 5. 功能需求（Epic + User Story + AC）

按 Epic 组织，每条 user story 用 "**As / I want / So that**" 格式，配 **Acceptance Criteria** + **Priority** + **依赖蓝图主题**。

### Epic 1 — 写路径生产化（蓝图 Theme A/B/H）

#### US-1.1 写路径性能达标

> **As** 平台 SRE，
> **I want** 写请求 p99 在 50ms 以内，
> **So that** 业务发布期峰值 50 QPS 写不会超时。

**AC**：
- [x] 3-node Linux LAN bench：write_only @ c=16，p99 ≤ 50ms，RPS ≥ 5 000
- [x] mixed_95r5w @ c=16，p99 ≤ 30ms
- [x] 低 QPS 场景 p50 退化 ≤ 5ms（ApplyBatch 不能伤害低负载体验）
- [x] 退化保护：SLO 违规时可热降级到 batch=1
- [x] 新增指标：`paladin_raft_apply_duration_seconds{phase=...}`、`paladin_raft_batch_size_total`

**优先级**：P0  **蓝图依赖**：Theme B  **风险**：见蓝图 §7 第 2、3 行

#### US-1.2 协议稳定可演进

> **As** SDK 维护者，
> **I want** wire 协议有版本号且只加不改字段，
> **So that** 我升级服务端时老 SDK 不会突然解析失败。

**AC**：
- [x] 所有 wire 数据结构定义在 `api/paladinpb/v1/*.proto`
- [x] Raft log payload = `[magic_byte=0x02] + proto.Marshal(OpEnvelope)`；老 JSON 格式（无头）兼容期 ≥ 6 个月
- [x] CI 集成 `buf lint` + `buf breaking`，破坏性变更必须显式打 `BREAKING:` tag
- [x] 一次性 compaction 演练剧本：预发跑过、回滚剧本可用

**优先级**：P0  **蓝图依赖**：Theme A

#### US-1.3 错误协议语义化

> **As** SDK 客户端开发者，
> **I want** 用错误码而不是字符串识别错误类型，
> **So that** 服务端调文案不会让我的 404 变 500。

**AC**：
- [x] 服务端所有错误走 `status.Error(codes.X, ...)`，HTTP gateway 自动映射状态码
- [x] CI lint 禁用 `err.Error() == "..."` 字符串比较模式
- [x] 错误码表入 `docs/error-codes.md` + 蓝图附录 B
- [x] SDK 提供 `errors.Is(err, paladin.ErrKeyNotFound)` 风格的 sentinel

**优先级**：P0  **蓝图依赖**：Theme H

---

### Epic 2 — 读路径与一致性（蓝图 Theme C）

#### US-2.1 默认快读保留

> **As** 微服务工程师，
> **I want** 大部分读请求在 5ms 以内返回，
> **So that** 我的请求路径不被配置查询拖慢。

**AC**：
- [x] Stale read p99 ≤ 5ms @ 500k RPS（3-node LAN）
- [x] 任意节点本地读，无 Raft 开销（保留 v0.x 行为）
- [x] 故障注入：follower apply lag ≤ 100ms

**优先级**：P0  **蓝图依赖**：Theme C

#### US-2.2 显式线性一致读

> **As** 发布平台开发者，
> **I want** "刚写完立刻读到"的语义保证，
> **So that** 我的灰度百分比不会因为读到旧值而误发布。

**AC**：
- [x] gRPC `Range(consistent=true)` / HTTP `?consistent=true` 两种入口
- [x] Consistent read p99 ≤ 15ms @ 50k RPS
- [x] 实现走 `raft.Barrier()` 模拟 ReadIndex（避免对 hashicorp/raft 强版本依赖）
- [x] 故障期间（leader 不可达）显式返 `LEADER_UNAVAILABLE`，不静默退化为 stale 读

**优先级**：P0  **蓝图依赖**：Theme C

---

### Epic 3 — Watch 真生产化（蓝图 Theme D）

#### US-3.1 Watch 不再静默丢事件

> **As** 微服务工程师，
> **I want** Watch 落后时收到显式错误而不是静默没事件，
> **So that** 我能确定地知道是"真没变更"还是"我掉队了"。

**AC**：
- [x] `WatchResponse` 含 `compacted=true` + `compact_revision` 字段
- [x] 客户端收到 `compacted` 后 SDK 自动触发 `FullSync`，业务层无感
- [x] 兼容期：v0.x SDK 连接老 long-poll 接口仍 work；新接口走 gRPC stream
- [x] 集成测试：制造 4096+ 写事件 → 慢 watcher 必收 `compacted`

**优先级**：P0  **蓝图依赖**：Theme D

#### US-3.2 Watch 性能与规模

> **As** 平台 SRE，
> **I want** 集群能稳定撑 10k 并发 watcher，
> **So that** 公司 10k 个 sidecar 全连上不会把 leader 打垮。

**AC**：
- [x] 10k 并发 gRPC watch stream 稳态运行 ≥ 1 小时；leader 内存 < 2 GB
- [x] Watch E2E 通知 p99 ≤ 200ms（写发起 → 客户端 OnChange 触发）
- [x] WatchCache 索引化：revision 二分 + prefix radix（O(log N + M) 替代 O(N)）
- [x] 协议从 HTTP long-poll 升级到 gRPC server-streaming（H2 多路复用）

**优先级**：P0  **蓝图依赖**：Theme D（核心）+ Theme F（gRPC 入口）

#### US-3.3 Snapshot 恢复后 Watch 不漏

> **As** 微服务工程师（被 follower 转发的 watcher），
> **I want** follower 装快照恢复后我能感知到状态变化，
> **So that** 我不会拿着发布前的旧配置一直跑。

**AC**：
- [x] 修复现有缺陷：FSM.Restore（`raft/node.go:359`）当前**绕过 WatchableStore.Put**
- [x] 修复方案：Restore 完成后 server 主动给所有 watcher 发 `compacted`
- [x] 集成测试：disconnect follower → 触发 1024+ 写 → reconnect → 装快照 → 验 watcher 收到 `compacted` 信号

**优先级**：P0  **蓝图依赖**：Theme D

---

### Epic 4 — 协调原语（蓝图 Theme E）

#### US-4.1 Lease（带 TTL 的 key）

> **As** 微服务工程师，
> **I want** 把 key 关联到 lease，lease 不续就自动删除，
> **So that** 我可以做"服务实例存活注册"，进程死了 key 自动消失。

**AC**：
- [x] gRPC `Lease.Grant / Revoke / KeepAlive`（bi-stream KeepAlive）
- [x] `PutWithLease(key, value, lease_id)`：lease 过期 → key 自动删 + 发 DELETE event
- [x] **切主后新 leader 必须从持久状态重建到期 heap**（最易错点，混沌必测 C13）
- [x] KeepAlive 限频：5s 一次走 Raft，避免每次都打 quorum

**优先级**：P1  **蓝图依赖**：Theme E

#### US-4.2 Txn / CAS

> **As** 发布平台开发者，
> **I want** 原子的 if-then-else 操作（CAS 是其子集），
> **So that** 多个发布平台实例同时改同一个开关时不会互相覆盖。

**AC**：
- [x] gRPC `KV.Txn(compare, success, failure)`
- [x] CAS = `If{mod_rev==X} Then{Put(k,v)}`；compare 失败显式返 `PRECONDITION_FAILED`
- [x] **判定与执行原子**（必须在 FSM.Apply 内做 compare）
- [x] 1k 并发 CAS 收敛性测试：所有客户端最终视图一致

**优先级**：P1  **蓝图依赖**：Theme E

#### US-4.3 Compaction / Defrag

> **As** 平台 SRE，
> **I want** 在线回收旧 revision 历史和 bbolt 空洞，
> **So that** 长时间运行的集群不会无限膨胀。

**AC**：
- [x] `gRPC Maintenance.Compact(revision)`：丢弃 ≤ revision 的事件历史
- [x] `gRPC Maintenance.Defrag()`：bbolt `Compact()`，仅 leader 在 QPS 低谷期跑
- [x] 在线运行不中断读写（混沌 C 类测试）

**优先级**：P1  **蓝图依赖**：Theme E

---

### Epic 5 — 部署与安全（蓝图 §4.1 + Theme F）

#### US-5.1 双协议入口

> **As** SDK 维护者，
> **I want** 主协议是 gRPC（H2 多路复用、类型安全），但 HTTP 兼容入口仍可用，
> **So that** 我能高效跑生产，又能 `curl` 调试。

**AC**：
- [x] `:2379` gRPC（借 etcd 端口惯例）
- [x] `:2381` HTTP via `grpc-gateway`，老 URL `/api/v1/config/...` 不变
- [x] H2 单连接同时跑 Get / Watch / KeepAlive
- [x] HTTP 接口 deprecated 但保留 ≥ 6 月

**优先级**：P0  **蓝图依赖**：Theme F

#### US-5.2 全链路 TLS

> **As** 平台 SRE，
> **I want** 客户端、节点间通信全是 TLS / mTLS，
> **So that** 我能放心放进 VPC，不怕被 sniff。

**AC**：
- [x] 默认开启 TLS；`--insecure` flag 显式开关（教学/本地用）
- [x] Peer 间（Raft transport）独立 mTLS 证书
- [x] 证书 rotation 不中断服务（cert-manager / Vault PKI 集成示例）
- [x] 过期前 7 天告警（metric + log）

**优先级**：P0  **蓝图依赖**：§4.1

#### US-5.3 鉴权

> **As** 平台 SRE，
> **I want** 接入认证机制，
> **So that** 不是 VPC 内任何 pod 都能改我的配置。

**AC**：
- [x] gRPC metadata / HTTP header 认证：`authorization: Bearer <jwt>` / `Basic ...`
- [x] 内置 provider：static users + JWT (HS256/RS256)
- [x] OIDC：P5 之后（v1.x 增量）
- [x] Token 失效 / 过期：返 `UNAUTHENTICATED`（401）

**优先级**：P0  **蓝图依赖**：§4.1

#### US-5.4 RBAC

> **As** 平台 SRE，
> **I want** 区分谁能写、谁只能读、谁能管理某 prefix，
> **So that** 单业务团队不会误改全局配置。

**AC**：
- [x] 对象：`User / Role / RoleBinding`
- [x] 最小粒度：`(verb, path_prefix)`，verb ∈ {read, write, watch, admin}
- [x] 默认角色：`root` / `read-only` / `tenant-admin`（限 prefix）
- [x] ACL 数据存 `__paladin/acl/` 前缀走 Raft 复制，故障切换无视
- [x] **紧急恢复**：保留 `root token` 通道；审计日志永不能被禁写

**优先级**：P0  **蓝图依赖**：§4.1

#### US-5.5 审计日志

> **As** 合规 / 安全工程师，
> **I want** 所有写和管理操作都有审计记录，
> **So that** 出事故能查谁在什么时候改了什么。

**AC**：
- [x] 字段：`ts, actor, src_ip, verb, path, before_rev, after_rev, status, trace_id`
- [x] Append-only 文件 + 可选 SIEM 对接
- [x] 审计日志写失败 = 服务降级到只读，不能静默丢
- [x] 默认保留 90 天（可配置）

**优先级**：P0  **蓝图依赖**：§4.1

---

### Epic 6 — 运维与可观测性（蓝图 §4.3 / §4.4）

#### US-6.1 可观测性栈

> **As** Oncall 工程师，
> **I want** 凌晨 3 点被 page 时能在 5 分钟内定位到问题模块，
> **So that** 我不用通宵看代码 grep 日志。

**AC**：
- [x] **Metrics**：RED + USE + Biz 三类（清单见蓝图 §4.3 表）
- [x] **Logs**：`log/slog` JSON，含 trace_id；CI lint 禁用 `log.Printf`
- [x] **Traces**：OTLP exporter；关键 span：`rpc.server / raft.apply / bolt.tx / watch.dispatch / sdk.full_pull`
- [x] **Health**：`/healthz`（liveness）、`/readyz`（readiness：leader 已知 + apply lag < 10s + bolt 可写）
- [x] **Grafana 大盘**：开箱即用 1 个，覆盖三类指标，提交到 `docs/dashboards/`

**优先级**：P0  **蓝图依赖**：§4.3

#### US-6.2 配置热更新

> **As** 平台 SRE，
> **I want** 调日志级别 / 改限流不重启进程，
> **So that** 排障期我不用打断在飞 watch 流。

**AC**：
- [x] 单一配置文件 `paladin.yaml`（蓝图 §4.4 schema）
- [x] `SIGHUP` 或 `/admin/reload` 触发热更新
- [x] **可热更**：observability、auth、log_level、quota
- [x] **必须重启**：listen 端口、TLS 证书路径、Raft 配置（election timeout 等）

**优先级**：P1  **蓝图依赖**：§4.4

#### US-6.3 Backup / Restore

> **As** 平台 SRE，
> **I want** 一致性快照导出和冷启动恢复工具，
> **So that** 数据出事故时我有回退路径。

**AC**：
- [x] `paladin-core backup --endpoint=... --out=snap.gz`（gRPC `Maintenance.Snapshot` + 校验和）
- [x] `paladin-core restore --snap=... --data-dir=... --initial-cluster=...`
- [x] **10 GB 端到端 ≤ 10 min**
- [x] 增量备份（v1.1 增量目标）

**优先级**：P0  **蓝图依赖**：§4.4

#### US-6.4 滚动升级

> **As** 平台 SRE，
> **I want** 升级集群对客户端无感，
> **So that** 业务不会因为我升级中间件而 oncall。

**AC**：
- [x] Follower → Leader 顺序升级，一次一个
- [x] `MoveLeader` API：升级 leader 前主动迁移
- [x] 客户端 p99 抖动 < 10ms
- [x] 演练通过：v0.9 → v1.0 滚动升级混沌剧本 C10

**优先级**：P0  **蓝图依赖**：§4.4

#### US-6.5 故障 runbook 与 inspect 工具

> **As** Oncall 工程师，
> **I want** 离线工具读 bbolt 状态、对照 runbook 走，
> **So that** 服务挂了我也能离线诊断。

**AC**：
- [x] `paladin-core inspect data.db`：列 keys、统计体积、查 raft-log.db index range
- [x] `docs/runbook/`：≥ 5 篇，覆盖 leader stuck / disk full / TLS 过期 / split brain / lease 风暴
- [x] 每个 metric / alert 对应一篇 runbook（90% 覆盖）

**优先级**：P0  **蓝图依赖**：§4.5

---

### Epic 7 — SDK 韧性（蓝图 Theme G）

#### US-7.1 Endpoint Pool 与健康感知

> **As** 微服务工程师，
> **I want** 单节点宕机时 SDK 自动切换，业务无感，
> **So that** 我不用为每次集群扰动写降级逻辑。

**AC**：
- [x] SDK 利用所有 `Addrs`，不再只用 `Addrs[0]`
- [x] 后台 health loop 每 2s 探测；连续失败 3 次标 unhealthy
- [x] EWMA p95 加权选 endpoint（慢节点自然减权）
- [x] **单节点宕机恢复 ≤ 3s**（混沌验证）

**优先级**：P0  **蓝图依赖**：Theme G

#### US-7.2 请求类型感知路由

> **As** 微服务工程师，
> **I want** SDK 自动把写发给 leader、把 stale 读分散到所有节点，
> **So that** 我不用关心拓扑。

**AC**：
- [x] 写：`KV.Put` → `LEADER_UNAVAILABLE` 自动重试 ≤ 3 次指数退避
- [x] Stale 读：任意健康 endpoint
- [x] Consistent 读：只发 leader；leader 切换中短暂 backoff
- [x] Watch / Lease：粘住一个 endpoint，断开自动重选

**优先级**：P0  **蓝图依赖**：Theme G

#### US-7.3 本地缓存升级

> **As** 微服务工程师，
> **I want** 缓存有 schema 版本和增量更新，
> **So that** 升级 SDK 时不会因为缓存格式不兼容直接挂。

**AC**：
- [x] 缓存文件含 `schema_version` 字段，不匹配拒绝使用
- [x] 增量持久化：watch 事件即刻更新本地副本
- [x] 损坏降级：保留 `.bak`；新缓存写失败回滚
- [x] 启动从缓存回放 `last_revision` 再 watch（节省全量拉取）
- [x] 保留 v0.x 的 SHA-256 校验

**优先级**：P0  **蓝图依赖**：Theme G

---

## 6. 非功能需求（NFR）

### 6.1 性能（量化）

详见 §4.1 SLO + 蓝图 §5 性能预算。本 PRD 的 NFR 是 SLO 的延伸：

- **稳定性**：所有 SLO 在 7×24 长跑（≥ 14 天）下持续达标
- **退化保护**：所有性能优化必须有"降级到上一档"的开关
- **基线对比**：每次发布对比上一版基线，退化超 §8.2 表格门槛 = block merge

### 6.2 可靠性

- 三节点集群可用性 ≥ 99.99%
- 单节点故障不影响整体（无单点）
- 数据零丢失：Raft quorum 持久化是写返回成功的前提
- 崩溃恢复：进程异常退出后，重启数据无损坏（依赖 bbolt + Raft log 双保证）

### 6.3 可扩展性

- 单集群支持 ≤ 10M keys、≤ 50 GB
- 单节点支持 ≤ 10k 并发 SDK 长连接
- 水平扩读：通过加节点（learner / voter）扩读吞吐
- 写不水平扩展（v1.0 明确单 leader）

### 6.4 安全（详见 Epic 5）

- TLS / mTLS 全链路
- RBAC 最小权限
- 审计日志不可绕过
- 通过外部渗透测试无高危项

### 6.5 可运维性（详见 Epic 6）

- 单二进制、单配置文件、零外部依赖（除证书与 PKI 基础设施）
- 全套 Prometheus 指标 + OTLP traces + 结构化日志
- 5 篇 runbook 覆盖 ≥ 90% 已知故障模式
- 滚动升级、热配置、backup/restore 工具齐全

### 6.6 兼容性

详见 §8。

### 6.7 合规

- 审计日志保留 ≥ 90 天可配
- 敏感字段（value、token）默认日志脱敏
- 依赖 license 合规（CI `go-licenses` 强制）
- CVE 扫描：CI `govulncheck` + nightly cron

### 6.8 工程规范（详见蓝图 §2.3）

- `gofumpt` / `golangci-lint` 套件常绿
- `go test -race` 100% 通过
- `goleak` 断言无 goroutine 泄漏
- 包覆盖率 ≥ 80%（新代码 ≥ 90%）
- CI < 10 分钟

---

## 7. 兼容性与迁移

### 7.1 兼容期承诺

| 维度 | 承诺 |
|---|---|
| HTTP 老路径（`/api/v1/config/...`） | v1.0 GA 后保留 ≥ 6 个月 |
| JSON wire（无 magic-byte 的 Raft log） | 兼容期至少跨一次 minor 版本 |
| v0.x SDK | 通过 HTTP gateway 继续可用 |
| 错误协议（v1.0 后） | 只加新错误码，不改旧码语义 |
| Revision 语义 | v1.0 后冻结 |
| BoltDB on-disk 格式 | 加版本号字段；新版本必须能只读旧格式（用于回滚） |

### 7.2 v0.x → v1.0 迁移路径

#### 路径 A — 全新部署（推荐）

1. v1.0 集群独立起 → 通过 v0.x 的 backup 工具导出 → v1.0 restore
2. 客户端逐步切流量
3. 验稳后下线 v0.x

#### 路径 B — 原地升级（高风险，仅限教学集群）

1. 备份 v0.x 数据
2. 触发蓝图 Theme A 的"一次性 compaction + snapshot 全量演练"
3. 滚动升级到 v1.0（按蓝图 §4.4 滚动升级剧本）
4. 验证后清除 JSON 兼容代码（v1.1 期）

### 7.3 破坏性变更清单（v0.x → v1.0 BREAKING）

- ❗ 默认开 TLS（`--insecure` 显式关）
- ❗ 默认开 Auth（`--no-auth` 显式关；仅限教学）
- ❗ Raft log payload 从 JSON → pb（兼容期外）
- ❗ 错误返回从 free-text 改为 status code（HTTP 状态码可能变化）
- ❗ 端口约定：gRPC 主端口 `:2379` 替代原 `:8080`

每条变更都要在 CHANGELOG 显式 `BREAKING:` 标记，UPGRADE.md 给出回滚路径。

---

## 8. 发布计划与里程碑

里程碑视角（产品/用户语言），具体 sprint 拆分见蓝图 §6 Phase 0-5。

### M1: 工程地基就绪（对齐蓝图 P0；3 周）

- **用户可见**：性能未变，但 `/metrics` 可被 Prometheus 抓取，日志变 JSON
- **关键交付**：proto schema、log/slog 全替、错误码、配置文件、linter 套件、OTLP
- **Go-NoGo**：`/metrics` 可抓 + bench 数字 ±5% 无回归

### M2: 写路径生产级（对齐蓝图 P1；4 周）

- **用户可见**：写 RPS 飙到 5k；mixed p99 大幅改善
- **关键交付**：ApplyBatch + group fsync + pb wire + 流式 snapshot
- **Go-NoGo**：US-1.1 / US-1.2 全部 AC 达标

### M3: 读 / Watch / SDK（对齐蓝图 P2；3 周）

- **用户可见**：consistent read 可用；Watch 不再静默丢；SDK 单点宕机无感
- **关键交付**：ReadIndex、gRPC Watch、SDK endpoint pool、grpc-gateway
- **Go-NoGo**：Epic 2、Epic 3、Epic 7 全部 AC 达标

### M4: 协调原语（对齐蓝图 P3；3 周）

- **用户可见**：可用 Lease 做服务发现、Txn/CAS 做并发控制
- **关键交付**：Lease + Txn + Compact + Defrag
- **Go-NoGo**：Epic 4 全部 AC 达标，混沌 C13 / C14 通过

### M5: 安全与运维（对齐蓝图 P4；3 周）

- **用户可见**：可放进 VPC 用了
- **关键交付**：TLS / mTLS + RBAC + 审计 + Backup/Restore + 滚动升级 + Grafana 大盘 + runbook
- **Go-NoGo**：Epic 5、Epic 6 全部 AC 达标，外部渗透测试无高危

### M6: GA 1.0（对齐蓝图 P5；2 周）

- **用户可见**：v1.0 release 公开
- **关键交付**：全部混沌剧本通过、14 天长跑达标、灰度 2 周观察、文档齐全
- **Go-NoGo**：上一节"v1.0 GA 客观判据"达成

### 总时长与缓冲

蓝图原估 18 人周，加 PRD 协调 / 验收 / 回归 buffer 20% → **约 22 人周（5.5 个月）**。

如团队是 2-3 人架构小组，**日历时间约 6-7 个月**。

### 决策点（Go-NoGo）

每个里程碑结束都是一个 Go-NoGo 决策点，参与方见 §11 RACI。NoGo 的两种处置：
1. 修复后再决策（默认）
2. 缩减下一里程碑范围（极端情况）

---

## 9. 依赖

### 9.1 技术依赖

| 依赖 | 版本 | 用途 | 风险 |
|---|---|---|---|
| `hashicorp/raft` | v1.7.x | 共识 | 已在线 |
| `etcd-io/bbolt` | v1.4.x | KV 存储 | 已在线 |
| `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` | latest stable | wire 生成 | M1 引入，CI 工具链负担 |
| `grpc-go` | latest stable | gRPC 服务 | M2 引入 |
| `grpc-ecosystem/grpc-gateway` v2 | latest | HTTP 兼容入口 | M3 引入 |
| `go.opentelemetry.io/otel` | latest | tracing | M1 引入 |
| `prometheus/client_golang` | latest | metrics | M1 引入 |
| `go-jose/go-jose` 或 等价 | latest | JWT | M5 引入 |
| `cert-manager` 或 Vault PKI | infra-side | 证书管理 | M5 集成示例（不是硬依赖） |

### 9.2 团队/组织依赖

| 依赖方 | 内容 | 关键时间点 |
|---|---|---|
| 安全团队 | 渗透测试、威胁模型审阅 | M5 末 |
| SRE 团队 | Grafana 接入、告警接入、值班培训 | M5 起持续 |
| 文档/技术写作 | runbook、tutorial、release notes | M5/M6 |
| QA / 测试 | 集成测试、混沌测试执行 | M3 起持续 |
| 法务 / 合规 | license 检查、审计要求确认 | M5 |

### 9.3 外部依赖（用户/集成方）

| 集成方 | 内容 | 时间点 |
|---|---|---|
| 灰度发布平台 | 集成 ReadIndex + CAS，做迁移试点 | M4 末，作为 dogfooding |
| 内部某中型业务 | 作为 beta tester，VPC 试点 | M5 末 |

---

## 10. 风险与缓解

详细风险矩阵见蓝图 §7。本 PRD 视角的"产品级风险"：

| ID | 风险 | 概率 | 影响 | 缓解 | Owner |
|---|---|---|---|---|---|
| R1 | 工期严重超期（> 8 个月） | 中 | 中 | 每月 review；超 20% 触发范围裁剪决策（先牺牲 Epic 4 的 Lease） | EM |
| R2 | 性能目标达不到（write p99 > 100ms） | 中 | 高 | M1 末跑预测试；不达标提前换 Theme B 实现路径 | 架构 |
| R3 | beta 客户反馈 v1.0 仍不能上 | 中 | 致命 | M5 起 dogfooding，每 2 周收反馈 | PM |
| R4 | 安全审计发现高危需返工 | 中 | 高 | M3 起请安全侧前置 review；M5 末才是正式渗透 | 安全 |
| R5 | 外部贡献者社区流失（"代码变复杂") | 中 | 中 | 守住 6000 行红线；README 持续更新；新功能加可读性 review | 架构 |
| R6 | 上游库（hashicorp/raft）出 CVE | 低 | 高 | `govulncheck` nightly + 升级演练 | SRE |
| R7 | bbolt 性能瓶颈在 NVMe 上仍不达标 | 中 | 高 | M2 末预留切换 Pebble 的 RFC 时间窗口 | 架构 |
| R8 | RBAC 配置错误锁死管理员 | 低 | 高 | 永久保留 root token 紧急通道 + 文档 | SRE |
| R9 | Lease 切主重建出 bug | 中 | 中 | 混沌 C13 必过 + 长跑必测 | 测试 |
| R10 | Wire 跃迁导致集群数据损坏 | 低 | 致命 | 预发演练 + 回滚剧本 + SHA256 校验 | 架构 |

---

## 11. RACI（角色与决策权）

| 工作项 | R (Responsible) | A (Accountable) | C (Consulted) | I (Informed) |
|---|---|---|---|---|
| PRD 内容 | PM | EM | 架构、SRE、安全 | 全员 |
| 工程蓝图（HOW） | 架构 | EM | 全员 | PM |
| 性能 SLO 验收 | 架构 + 测试 | EM | SRE | PM |
| 安全验收 | 安全 | EM | 架构 | PM、SRE |
| 滚动升级剧本 | SRE | EM | 架构 | PM |
| 兼容性破坏决策 | 架构 + PM | EM | 安全、SRE | 全员 |
| Go-NoGo（每里程碑） | EM | EM | PM、架构、SRE、安全 | 全员 |
| Beta 客户对接 | PM | EM | 架构、SRE | 全员 |
| 公开 release | PM + EM | EM | 全员 | 全员 |

---

## 12. 开放问题

引用 `TODOS.md` 中已识别的 OV 系列开放问题，是 v1.0 必须再决策的：

| ID | 问题 | 触发再评估时间 | 默认处置 |
|---|---|---|---|
| OV3 | porcupine 不能验崩溃一致性 | M5 写 release notes 前 | 收紧"无数据丢失"承诺为"linearizability verified" |
| OV4 | gRPC Watch 是否真值得 10 人日 | M1 末，量 HTTP long-poll 在 Linux 上的真实 p99 | 若 < 40ms，把 Theme D 的 gRPC stream 推到 v1.1 |
| OV9 | "配置中心"定位是否陷阱 | M5 README headline 改之前 | 改为"轻量、可审计、线性一致 KV，适用于配置工作负载" |
| OV10 | 6 个月对 pre-v1 OSS 是否过长 | M2 末若无外部用户兴趣 | 砍掉 M4（Lease/Txn）做 v0.9 早发，Lease/Txn 推到 v1.1 |

新增本 PRD 暴露的开放问题：

| ID | 问题 | 触发再评估时间 |
|---|---|---|
| OV11 | "1 个真实业务运行 90 天"是否过严？是否应改为"30 天 + dogfooding 通过" | M5 末 |
| OV12 | RBAC 的"路径前缀"粒度是否够？是否需要"按 key 模式" | M5 起灰度反馈 |
| OV13 | 默认开启 TLS / Auth 是否会让教学集群门槛过高 | M5 末，看 quickstart 反馈 |

---

## 13. 附录

### A. 名词表

| 术语 | 含义 |
|---|---|
| Revision | 全局单调递增计数器；每次写 +1（包括 Delete） |
| FSM | Raft 状态机；接收 committed log entry 应用到本地存储 |
| Watch | 客户端订阅 key/prefix 变更的机制 |
| Lease | 带 TTL 的资源句柄；过期则关联 key 自动删 |
| ReadIndex | etcd 论文里的线性一致读机制；本项目用 Barrier 模拟 |
| Compacted | Watch 起始 revision 已被回收，客户端必须 FullSync 重开 |

### B. FAQ（用户视角）

**Q: PaladinCore v1.0 vs etcd 怎么选？**
A: 你的数据 < 50 GB、写 < 5k QPS、要的是"读得懂改得动"——选 PaladinCore。其他场景选 etcd。

**Q: 我能从 etcd 迁移过来吗？**
A: v1.0 不提供迁移工具。但 wire 语义对齐 etcd v3，自己写迁移脚本不难（约 100 行 Go）。v1.1 路线图考虑提供。

**Q: 支持 Java / Python SDK 吗？**
A: v1.0 只有 Go SDK。但 gRPC 协议公开，第三方 SDK 可用 protoc 生成。v1.1 路线图考虑官方 Java SDK。

**Q: 能跨 region 吗？**
A: **不能**。v1.0 显式不支持。要跨 region 用 CockroachDB / Spanner / etcd Learner。

**Q: 怎么备份？**
A: `paladin-core backup --endpoint=... --out=snap.gz`。详见 US-6.3。

### C. 引用文档

- 工程蓝图：`docs/production-refactoring.md`
- v2 范围：`docs/paladincore-v2-spec.md`
- 开放问题：`TODOS.md`
- 现状：`README.md`
- 性能基线：`bench/baselines/v0.1/`
- 系列教程：`docs/series/00-07*.md`
- 面试训练材料：`interview.md`

### D. 变更日志

| 日期 | 版本 | 变更 | Owner |
|---|---|---|---|
| 2026-05-08 | v1.0-draft-1 | 初稿 | PM |

---

## 结语

> 这份 PRD 不是给"什么都要"的产品经理写的。它是给一个**正在认真把教学项目推进到生产**的小团队写的——所以它的价值是：
>
> 1. **告诉你不做什么**（§3.3、§10、蓝图附录 D）。
> 2. **告诉你如何判断"做完了没有"**（每个 user story 的 AC、§4 SLO、§8 Go-NoGo）。
> 3. **告诉你"用户视角"和"工程视角"在哪里对应**（每个 Epic 引用蓝图主题）。
>
> 工程蓝图回答"怎么做"。本 PRD 回答"做对了没有"。两者都不可少。
