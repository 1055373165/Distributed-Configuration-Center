# PaladinCore 源码蒸馏专栏

> 一个两千行、七天建成的分布式配置中心,把 etcd / Raft / 长轮询 / SDK 降级 这些
> "在别处只能仰视" 的概念,在你自己的仓库里拆开、跑通、再装回去。

本系列按 [SPARK V2](../../源码蒸馏.md) 写作法组织:每一篇都以**认知冲突**开篇、用
**苏格拉底提问链**穿透本质、以**多层类比**建立心智模型、用**源码考古 + 渐进揭示**
给出实现证据,再以**最小可运行实验(MRE)**把结论落到地上,最后提炼**决策图谱**并
给出**跨域迁移**建议。七篇自成一条主线,也互相独立。

## 阅读节奏

每一天解决的是"上一天遗留的一个具体缺陷",因此按顺序读收益最大:

```
Day0 为什么需要 → 建立坐标系(CAP / 市面对比)
Day1 单机 KV → 没有网络
Day2 + HTTP  → 没有推送
Day3 + Watch → 单点故障
Day4 + Raft  → 客户端得知道 Leader
Day5 + Forward → 客户端还得自己维护连接/缓存
Day6 + SDK   → 生产部署没解决
Day7 + 集群  → 回到"它凭什么是配置中心"的完整答卷
```

## 章节索引

| # | 篇名 | 解决的矛盾 | 关键机制 | 对应源码 |
|---|------|-----------|---------|---------|
| 00 | [为什么需要配置中心](./00-why-configuration-center.md) | 单体→微服务的配置失控 | CAP 坐标 / 七大产品对比 | — |
| 01 | [Revision:用一个递增计数器当逻辑时钟](./01-revision-kv-store.md) | 物理时钟不可靠 / Watch 需要时序 | BoltDB 事务内 rev++ | `store/bolt.go` |
| 02 | [三级 Key 与 HTTP 语义:URL 是资源,不是参数](./02-http-tenant-api.md) | 扁平命名冲突 / 幂等与状态码 | `tenant/ns/name` + 201/200 区分 | `server/server.go` |
| 03 | [环形缓冲 + sync.Cond:把轮询压成 RTT](./03-watch-ring-buffer.md) | Pull 延迟与带宽浪费 | `count % cap` + Broadcast | `store/watch.go` |
| 04 | [Raft FSM:把写操作从"单机调用"抽象为"日志事件"](./04-raft-fsm.md) | 单点故障 vs 一致性 | HashiCorp Raft + Bolt FSM | `raft/node.go` |
| 05 | [ForwardRPC 与 ReadIndex:客户端凭什么不知道 Leader](./05-forward-consistent-read.md) | LB 随机路由到 Follower | HTTP 代理 + VerifyLeader | `server/raft_server.go` |
| 06 | [SDK 三阶段生命周期:启动 / 运行 / 关机,缺一不可](./06-sdk-fallback.md) | 服务端不可达时如何不 panic | 全量拉取 + 长轮询 + SHA-256 缓存 | `sdk/client.go` |
| 07 | [从脚本到集群:本地三节点拓扑的最小可复现环境](./07-cluster-deploy.md) | 如何让"复现 bug"变成一条命令 | `cluster-local.sh` + Raft 自举 | `scripts/`, `cmd/` |

## 配套实验

每篇都带一个可复现的 Experience 套件,组合起来就是一份"从零运行整个系统"的 playbook:

```bash
# 起三节点集群(Docker 替代)
./scripts/cluster-local.sh --fresh
# 或指定端口避开冲突
HTTP_BASE=18080 RAFT_BASE=19001 ./scripts/cluster-local.sh

# 跑全量测试
go test ./... -race -count=1

# 停止 & 清数据
./scripts/cluster-stop.sh --clean
```

## 读者可以用到什么

这套专栏不是教你"抄一个配置中心",它的真正产物是四组可以搬到任何分布式系统里的思维工具:

- **逻辑时钟 vs 物理时钟** — 任何需要"因果序"的系统都要先想这一条(Day 1)
- **推 vs 拉 / 长轮询 vs 双向流** — 所有"客户端希望被动接收更新"的问题都绕不开(Day 3)
- **共识 vs 可用性 / 脏读 vs 线性一致** — Raft 不是银弹,它逼你在读路径上明确每一次选择(Day 4-5)
- **启动/运行/关机三段式 + 降级路径** — 任何一个"在生产 SLO 下能活"的客户端库的基本形状(Day 6)

---

> 源码是真理,但真理需要翻译。这一系列就是把 PaladinCore 这份两千行源码,翻译成一段你可以复述、可以动手、可以迁移的知识。
