# Raft FSM:把写操作从单机调用抽象为日志事件

> Day 4 · PaladinCore 源码蒸馏系列 04
>
> 单机跑得再精彩,进程一挂就是零。把"函数调用"翻译成"日志事件"是让单机走向集群的唯一窄门。

---

## 1. 引子:一次最贵的进程崩溃

Day 1-3 的单机版在本地跑很好,但凡事挡不住一次 OOM。进程挂掉的那一刻,所有订阅 Watch 的客户端同时失联,所有服务拿不到最新配置,所有依赖这份配置的上线流程必须手动接管。**单点故障的代价不是数据丢失,而是业务停摆**——这是从单机到集群最现实的推力。可一旦引入多节点,立刻撞上另一堵墙:**谁说了算?** 多台机器同时接受写请求,同一个 key 在不同机器上有不同值,revision 的单调性彻底塌方。这一天我们引入 Raft,解决方式不是"让所有机器都能写",而是把写操作从"函数调用"抽象成"日志事件",由 Leader 把日志复制到多数派、多数派确认后才 apply 到本地存储。

## 2. 四层问题

**是什么.** Raft 是一个共识算法,在 PaladinCore 里它把"Put/Delete 调用"变成"Raft log entry",确保所有节点以**相同顺序**应用相同的 entry。我们用 HashiCorp 的 `raft` 库(`raft/node.go:62-134`),自己只实现 `raft.FSM` 接口的三个方法:`Apply`、`Snapshot`、`Restore`。

**怎么做.** 写请求进 Leader,序列化成一个 `Op{Type, Key, Value}` 的 JSON(`raft/node.go:30-36`),调 `n.raft.Apply(data, timeout)` 丢进 Raft 日志;Raft 把这条 entry 复制到多数 Follower,收到 quorum ACK 之后标记为 committed;Committed 的 entry 被 Raft 框架送进我们实现的 `FSM.Apply`,在那里我们真正调用底层 `BoltStore.Put`。Follower 拿到被复制下来的 entry 后会走同一个 `FSM.Apply` 路径——这就是"所有节点以相同顺序应用相同日志"的具体机制。

**为什么.** 为什么是"复制日志 + 应用",而不是"多主复制 + 冲突解决"?因为配置中心需要**强一致**——同一个 key 在同一时刻必须只有一个值。多主复制要么用最后写入胜出(丢数据)、要么用 CRDT(值语义受限)、要么用版本向量(复杂度爆炸)。Raft 用一条"只能 Leader 写,其他人服从"的严苛规则,让整个系统退化成"分布式单线程",revision 的单调性继续成立。

**放弃了什么.** 放弃了"写的水平扩展"。3 节点 Raft 集群的写吞吐和单机差不多(Leader 只有一个),想扩写就得分片(每个分片一个 Raft group,像 TiKV)。但对配置中心这是可接受的:写少读多、对延迟的敏感度远低于对**不丢配置**的敏感度。

## 3. 类比

**生活类比.** Raft 就像公司里的会议记录:所有决议由会议主持人记在同一本**会议纪要**上,其他参会者每人复印一份(保证主持人失联时记录仍在);主持人宣布"这条写入成功"的前提是**至少半数以上与会者确认收到**。FSM.Apply 就是会后各部门按纪要去执行——每个部门拿到的是同一本纪要,所以执行结果一致。

**专业类比.** Raft 是"复制状态机"(Replicated State Machine)模式的现实落地:所有节点持有**同一份确定性状态机**,输入的是**同一串日志**,输出的是**同一个状态**。这个模式在数据库里叫"WAL + 主从同步",在区块链里叫"交易顺序 + 执行",在 Kafka 里叫"ISR + log replication"。理解了这个模式,所有分布式数据库看起来都是它的变体。

**精妙类比.** 把 Raft 想象成一个"乐队指挥 + 乐手"系统。指挥(Leader)面前的乐谱(Log)是权威版本,他用指挥棒(AppendEntries RPC)告诉每个乐手下一个该弹的音符;过半数乐手就位了(quorum),才允许他点头(commit);每个乐手拿到同一个音符按同一个节拍弹(FSM.Apply)。即使指挥突然晕倒,最资深的那位乐手(当选新 Leader)会接过指挥棒,乐谱已经在大家手里,演出不会中断。

**反类比.** Raft **不是**数据复制协议,它是**日志复制协议**。它复制的是"要做什么"的决定,不是"做完之后的数据"。如果你把它误解为"把数据库文件同步到另一台机器",就会去问"可不可以只复制差异"——答案是不行,因为差异复制无法保证确定性重放。所有节点必须经历**完全相同的状态转换序列**才能得到相同结果。

## 4. 揭秘

**数据结构.** `Op` 只有三个字段(Type/Key/Value),是整个 Raft 层和业务层的唯一耦合点。任何新操作(比如 CAS)都要在这里加一个 Type,然后在 FSM.Apply 里 switch 多一个分支——横向扩展的粒度非常明确。

**FSM.Apply 必须不阻塞.**

```raft/node.go:286-316
// CRITICAL: Apply must not block. It runs on the Raft apply goroutine.
// Any blocking here stalls the entire Raft state machine.
type FSM struct {
    store *store.WatchableStore
}

func (f *FSM) Apply(log *raft.Log) interface{} {
    var op Op
    if err := json.Unmarshal(log.Data, &op); err != nil {
        return &OpResult{Error: fmt.Sprintf("unmarshal op: %v", err)}
    }

    switch op.Type {
    case "put":
        result, err := f.store.Put(op.Key, op.Value)
        if err != nil { return &OpResult{Error: err.Error()} }
        return &OpResult{Entry: result.Entry, PrevEntry: result.PrevEntry}
    case "delete":
        deleted, err := f.store.Delete(op.Key)
        if err != nil { return &OpResult{Error: err.Error()} }
        return &OpResult{Entry: deleted}
    default:
        return &OpResult{Error: fmt.Sprintf("unknown op type: %s", op.Type)}
    }
}
```

这段代码能通过审查,靠的是两条潜规则:**Apply 里不能有网络 IO,也不能有可能卡死的锁**。BoltDB 的本地磁盘写是可预期的(毫秒级),所以是安全的;如果你把存储换成 "远程 Redis",任何网络抖动都会让整个 Raft 状态机 stall 住,一次 timeout 就触发重新选举,整个集群直接抖飞。这是"**FSM.Apply 为什么必须是本地、纯函数式**"的工程约束。

**Leader 写入路径.** `raft/node.go:138-163` 的 `Apply` 方法把业务 Op 交给 Raft:

```raft/node.go:138-163
func (n *Node) Apply(op Op, timeout time.Duration) (*OpResult, error) {
    if n.raft.State() != raft.Leader {
        return nil, ErrNotLeader   // 非 Leader 直接拒绝,Day 5 会加转发
    }
    data, _ := json.Marshal(op)
    future := n.raft.Apply(data, timeout)   // 丢进 Raft 日志,future 代表"这条 entry 被 commit+apply 之后的结果"
    if err := future.Error(); err != nil {
        return nil, fmt.Errorf("raft apply: %w", err)
    }
    result := future.Response().(*OpResult)  // 这个 Response 就是 FSM.Apply 的返回值
    return result, nil
}
```

`future.Response()` 的返回值就是上面 `FSM.Apply` 的 `interface{}` 返回值——Raft 库帮你把"日志被 commit"和"FSM 应用结果"这两件事粘在同一个 future 上,代码读起来像本地调用,底层其实走了一圈多数派复制。

**Snapshot 为什么必不可少.** Raft 日志不能无限增长,必须定期压缩。我们在 `raft/node.go:81` 配置了 `SnapshotThreshold = 1024`:每 1024 条 entry 之后,框架会调 `FSM.Snapshot()` 把当前状态序列化持久化,然后截断已 apply 的日志。新节点加入时如果日志落后太多,Leader 直接发 Snapshot + 之后的增量日志——这正是 Redis AOF rewrite、MySQL binlog purge 的同一条思路。

## 5. Experience

**MRE.** 单节点 bootstrap + 写入验证:

```bash
go test ./raft/ -v
# --- PASS: TestBootstrap            节点自选为 Leader
# --- PASS: TestPutAndGet             Put -> Raft -> FSM -> BoltDB
# --- PASS: TestDelete                Delete 经过 Raft
# --- PASS: TestRevisionMonotonic     revision 依然单调
# --- PASS: TestNotLeaderError        非 Leader 写入报错
```

**参数旋钮 1:FSM.Apply 挂个 sleep 看什么会坏.** 在 FSM.Apply 里加一行 `time.Sleep(5 * time.Second)`,然后跑 `TestPutAndGet`——你会看到 `future.Error()` 返回 "timeout enacting log"。这是 Raft 的自我保护:FSM 太慢,Leader 会认为自己"无法及时 apply",进而触发重新选举。负面实证正面定律:**Apply 必须快**。

**参数旋钮 2:SnapshotThreshold 与日志截断.** 把 1024 改成 10,连续写 50 条配置,观察 `data-<id>/snapshots/` 目录——会出现 5 个以上的快照文件。再查看 `raft-log.db` 的大小,会看到老日志被清理,文件不再无限增长。

**极端场景:单节点 bootstrap 后再退出,重启能否恢复?** PaladinCore 在 `raft/node.go:116-126` 的 `BootstrapCluster` 只在首次调用时生效——Raft 日志已存在时会被忽略。重启后节点从磁盘上的 log + snapshot 恢复到关机前的状态,revision、数据、集群成员全部原样。这是"有状态服务可重启"的基础。

**Benchmark.** 单节点 Raft 的写吞吐大致是单机 BoltDB 的 60-80%(多了一次 Raft log 持久化 + fsync)。三节点集群里,Leader 写仍然只受自己磁盘 fsync 限制,复制走 TCP 是异步并行的,延迟约在 0.5-1 ms 内网 RTT 级别。

## 6. Knowledge

**一句话本质.** Raft 把"写操作"替换成"写日志",让"谁执行"这个问题退化成"谁是 Leader"——共识问题被压缩到"Leader 选举 + 日志复制"两件事。

**决策树.**

```
需要高可用且强一致?
├─ 否(最终一致够用) → 异步主从 / Gossip
└─ 是 → 可接受单 Leader 写瓶颈?
        ├─ 否 → 分片 Raft(TiKV 模型:多个 Raft group)
        └─ 是 → 单 Raft group(etcd / PaladinCore 模型)
                └─ FSM.Apply 是否纯本地、幂等、不阻塞?
                    ├─ 否 → 先重构为纯本地操作,再接 Raft
                    └─ 是 → 可以接 Raft
```

**知识图谱.**

```
Client -> Leader.Apply(Op)
                │
                ↓  json.Marshal
           raft.Apply(data)
                │
                ├─→ Leader log + AppendEntries RPC -> Followers
                │         │
                │         └─→ quorum ACK
                │
                ↓  Leader 标记 committed
          FSM.Apply(log) 在所有节点串行执行
                │
                ↓
         BoltStore.Put / Delete + WatchCache.Append
                │
                ↓
           future.Response() 返给 Leader 的调用者
```

## 7. Transfer

**相似场景.** etcd、Consul、CockroachDB、TiKV 全是 Raft 的使用者;HDFS 的 JournalNode、Elasticsearch 的 cluster state、Kafka 的 Controller(3.x 之后切到 KRaft)也是同一个脑回路。理解 PaladinCore 的 FSM 模式后,这些系统的"写路径"你基本都能猜对。

**跨域借鉴.** 前端的 Redux 也是"action → reducer → state" 的**单线程日志应用模型**——action 类比 Op,reducer 类比 FSM.Apply,state 类比 WatchableStore。Raft 只是在这个模式外包了一层"把 action 广播给多个 reducer 并保证顺序一致"。

**未来演进.** 当你要追求更高读吞吐,自然会走向"Follower 提供读"——但要回答"读到的是不是最新的"。下一篇的 `VerifyLeader` / `ReadIndex` 就是对这个问题的两种工程答卷。

## 8. 回味

记忆锚点:**口诀"Op 化写入,日志当真相;Apply 不阻塞,Snapshot 压日志"**;**一张"Client → Leader → Raft Log → Quorum ACK → FSM.Apply → Store"的流程图**;**决策树:看读写比决定单 Raft 还是分片 Raft**。

**下一步.** 当前实现有个硬伤:客户端必须知道"谁是 Leader"才能发写请求,否则会吃到 `ErrNotLeader`。这在 LB 随机路由的真实部署里是灾难——下一篇我们在 Follower 侧实现 ForwardRPC,让客户端透明无感。

---

> **上一篇** ← [03. 环形缓冲 + sync.Cond](./03-watch-ring-buffer.md)
> **下一篇** → [05. ForwardRPC 与 ReadIndex:客户端凭什么不知道 Leader](./05-forward-consistent-read.md)
