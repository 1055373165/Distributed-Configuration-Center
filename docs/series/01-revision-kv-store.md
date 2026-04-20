# Revision:用一个递增计数器当逻辑时钟

> Day 1 · PaladinCore 源码蒸馏系列 01
>
> 当你以为配置中心只是 `map[string][]byte`,revision 会把这个天真的想法戳穿。

---

## 1. 引子:一个反直觉的事实

多数人初学配置中心时,脑子里画的是一个巨大的 `map[string][]byte` 再套层 HTTP。但只要你想做"变更推送",就必须能回答"自上次 X 之后发生过哪些事"——而这个 X 不能是 `time.Now()`,因为分布式里物理时钟会 NTP 回拨、会闰秒、节点之间还可能有分钟级偏差。于是得换一把尺子:一个由服务端单一写入者递增、只进不退的整数。这把尺子,在 etcd、ZooKeeper、PaladinCore 里都叫同一个名字——**revision**。本篇解决的核心矛盾就在这里:`map` 不够,`rev++` 为何是最小正确答案。

## 2. 四层问题:从现象到权衡

**Level 1(是什么).** revision 是一个全局单调递增的无符号整数,每一次成功的 `Put` 或 `Delete` 都会让它加一;每一条 `Entry` 都会把当时的 revision 快照进自己的字段里。PaladinCore 把这一对抽象写在了 `store/store.go:29-36` 的 `Entry` 定义里,它不只有一个版本号,而是**四个**:

```store/store.go:29-36
type Entry struct {
	Key            string `json:"key"`
	Value          []byte `json:"value"`
	Revision       uint64 `json:"revision"`
	CreateRevision uint64 `json:"create_revision"`
	ModRevision    uint64 `json:"mod_revision"`
	Version        uint64 `json:"version"`
}
```

**Level 2(怎么做).** 关键在**这四个字段如何被一致地维护**。答案在 `store/bolt.go:68-131` 的 `Put`:读旧值、递增 revision、写入新值、持久化 revision 四步**必须在同一个 BoltDB 事务内完成**。事务未提交时的崩溃保留旧状态,一旦提交四件事同时可见。

**Level 3(为什么这样).** 为什么不能"先写数据、再更新 revision"?因为一旦两次写入之间崩溃,要么出现"数据已写但 rev 没涨"(变更对 Watch 消失),要么"rev 涨了但数据没写"(重启后 rev 回退,新写入撞上已用号码,单调性塌方)。事务的作用就是把两件事压进一个**原子点**。

**Level 4(放弃了什么).** 代价是 BoltDB 的单写入者模型——写吞吐受限于单机 fsync(千次/秒量级)。但配置变更天然低频,且 Day 4 引入 Raft 后只有 Leader 写,与 BoltDB 的单写模型**约束对齐**而非叠加——Raft 本就要求单点写入构造全序日志。

## 3. 类比:三层心智模型

**生活类比.** revision 像银行柜员叫号——号码严格递增不回退不跳号,账簿记"号码 42 存入 500",凭号码就能回到那一刻的状态。PaladinCore 里号码是 `Revision`,账簿是 BoltDB。

**专业类比.** revision 等价于数据库的 MVCC 版本号。InnoDB 的 `DB_TRX_ID`、PostgreSQL 的 `xmin/xmax`、etcd 的 revision 都是同一条思路:不覆盖历史、用版本号定位时刻。`CreateRevision` 对应"行首次写入版本",`ModRevision` 对应"最新修改版本",`Version` 对应"被改过几次"——理解 MVCC 就理解为什么单个 `Revision` 不够。

**精妙类比.** revision 是 Lamport Clock 的**单写者退化形态**。当只有一个写者时,Lamport Clock 退化为一个整数计数器。因果性不需要物理时间,只需要一个**事件排序函数**——Leader 就是这个函数的唯一调用者。

**反类比.** revision 不是时间戳(不说"何时",只说"先后");不是 Git hash(无法溯源内容);不是自增主键(只本表唯一而非全库单调)。混淆这三者会让你犯"按时间戳查事件"这种一眼错的设计。

## 4. 揭秘:渐进式展开源码

**从数据结构开始.** 除了 `Entry` 的四字段,还有一处关键:`PutResult` 同时返回 `Entry` 和 `PrevEntry`(`store/store.go:39-42`)——这不是冗余,是给 Day 3 的 Watch 留的"旧值"接口,消费者做差异比较必须同时知道新旧值。

**函数签名.** `Store` 接口(`store/store.go:46-59`)只暴露五个动词:`Put/Get/Delete/List/Rev`。没有 `Update`、没有 `Set`——`Put` 一个方法包办创建和更新,区别靠返回值里 `PrevEntry` 是否为空。

**核心逻辑:Put 的事务内部.** 这段代码读懂,整个 Day 1 就懂了大半:

```store/bolt.go:74-128
err := s.db.Update(func(tx *bolt.Tx) error {
    data := tx.Bucket(bucketData)
    meta := tx.Bucket(bucketMeta)

    // 第 1 步:读旧值。没有旧值 -> 首次创建;有旧值 -> 继承 CreateRevision,递增 Version
    var prev *Entry
    if raw := data.Get([]byte(key)); raw != nil {
        prev = &Entry{}
        if err := json.Unmarshal(raw, prev); err != nil {
            return fmt.Errorf("unmarshal prev entry: %w", err)
        }
    }

    // 第 2 步:全局 revision++。这一步必须在事务内,后面第 4 步会把它持久化。
    newRev := s.rev + 1

    // 第 3 步:构造新 Entry,区分"首次创建"和"更新"两种分支
    entry := &Entry{Key: key, Value: value, Revision: newRev}
    if prev != nil {
        entry.CreateRevision = prev.CreateRevision  // 首次创建时机不变
        entry.ModRevision = newRev                   // 最新修改就是现在
        entry.Version = prev.Version + 1             // 改了多少次
        result.PrevEntry = prev
    } else {
        entry.CreateRevision = newRev                // 首次出现,Create/Mod 相等
        entry.ModRevision = newRev
        entry.Version = 1
    }

    // 第 4 步:数据和 revision 元信息一起写入,事务保证原子性
    encoded, _ := json.Marshal(entry)
    if err := data.Put([]byte(key), encoded); err != nil { return err }
    var revBuf [8]byte
    binary.BigEndian.PutUint64(revBuf[:], newRev)
    if err := meta.Put(keyRev, revBuf[:]); err != nil { return err }

    s.rev = newRev    // 只有事务即将提交时才更新内存缓存
    result.Entry = entry
    return nil
})
```

**优化细节一:Delete 也 bump revision.** `store/bolt.go:154-194` 的 Delete 事务里同样有 `newRev := s.rev + 1`。因为 Watch 消费者需要看到**删除事件**——如果删除不涨 rev,长轮询客户端下一轮问的是"> 10 的事件",服务端根本没生成过,删除就永远对客户端不可见。

**优化细节二:启动时从磁盘恢复 revision.** `store/bolt.go:54-65` 在 `NewBoltStore` 里从 `bucketMeta` 读出上次 shutdown 时的 rev 装回内存,保证**进程重启不让 revision 回退**。否则重启后 rev 从 0 起,很快撞上用过的号码,任何"按 rev 定位事件"的机制都会塌方。

## 5. Experience:自己跑一次

**最小可运行示例.** 一条命令验证 revision 的所有核心不变量:

```bash
cd paladin-core
go test ./store/ -run TestRevision -v
```

预期输出里,`TestRevisionMonotonic` 会验证每次 `Put` revision 严格递增,`TestRevisionPersistence` 会验证关闭再重开之后 revision 从断点恢复而不是从零开始。

**参数旋钮 1:Version 的累加.** 连续改同一 key,观察 `version` 递增:

```bash
go run ./cmd/paladin-core put app/db_host 10.0.0.1   # rev=1 version=1
go run ./cmd/paladin-core put app/db_host 10.0.0.2   # rev=2 version=2 prev=10.0.0.1
go run ./cmd/paladin-core put app/db_port 3306        # rev=3 version=1  (新 key)
```

`db_port` 虽然全局 rev 已到 3,自己的 `version` 仍是 1——这就是四字段各司其职。

**参数旋钮 2:删除也涨 rev.** 把 `store/bolt.go:177` 的 `newRev := s.rev + 1` 注释掉再跑 `go test ./store/...`,Day 3 的 `TestWatchableStoreDeleteEmitsEvent` 会立刻挂——这是最直接的负面实验。

**极端场景:并发 Put.** BoltDB 的 `db.Update` 会串行化写事务,但我们在外层还加了 `sync.Mutex`(`store/bolt.go:69`)。原因是 `s.rev = newRev` 修改的是 `BoltStore` 结构体字段而非 BoltDB 内部状态,BoltDB 管不到。去掉这把锁跑 `go test -race`,race detector 会立刻抓到。

**Benchmark.** `go test -bench=. ./store/` 显示单事务 Put 吞吐 1-5 万 ops/s(磁盘 fsync 决定);把 key/value 各扩 10 倍吞吐量级不变——瓶颈在 fsync 而不是序列化。

## 6. 知识:一句话本质 + 决策图谱

**一句话本质.** Day 1 解决的是"配置中心凭什么能精确回答'自 X 以来发生过什么'"这个问题,最小正确答案是**把 revision 递增和数据写入放进同一个 BoltDB 事务**——原子性是因果可追溯的前提,BoltDB 的单写事务把这件事做到了零额外代码。

**关键创新的副作用.** revision 的单调递增看似简单,它的副作用是整套设计从此和"多主写入"彻底绝缘——任何企图引入第二个写者的尝试都会立刻破坏 revision 单调性。这反过来解释了为什么 Day 4 必须引入 Raft 而不能直接搞多主复制:不是因为我们没考虑多主,而是因为 revision 的语义天然要求"单一事件序列",这和 Raft 的"Leader 是唯一日志追加者"是同一个约束的两种表达。

**决策树(何时用 / 何时不用).** 当你要在下一个项目里复用这个模式时,问自己三个问题足以定位适用性:

```
需要"自 X 以来发生过什么"的能力?
├─ 否 → 不需要 revision,用普通 map 即可
└─ 是 → 写操作由单一节点串行化?
        ├─ 否(多主写入) → revision 不适用,考虑 CRDT 或向量时钟
        └─ 是(单写 Leader / 数据库主节点 / 单进程)
            → 进一步:读需要看到"某一时刻的快照"?
                ├─ 是 → 用 MVCC 版本号(revision + CreateRevision + ModRevision)
                └─ 否 → 一个 revision 字段即可,etcd 的 Entry 退化为三字段
```

**知识图谱.**

```
BoltDB 事务 ──保证──→ 原子性
                        │
                        ↓
                   revision++ 与数据写入同生共死
                        │
    ┌───────────────────┼───────────────────┐
    ↓                   ↓                   ↓
 单调性(重启不回退)   因果性(谁先谁后)   可 Watch(> N 的事件)
    │                   │                   │
    └───────────────────┴───────────────────┘
                        │
                        ↓
                  Day 3 WatchCache 的输入前提
```

## 7. 迁移:举一反三

**相似场景复用.** 任何需要"客户端定位增量变化"的系统都可以套这套思路:消息队列里的 `offset`(Kafka consumer group 的 commit offset)、日志流里的 LSN(PostgreSQL WAL)、版本控制系统里的 commit hash 链。它们都在回答同一个问题:"从 X 开始,给我之后发生过的所有事件。"如果你理解了 revision,就能用同一个模板去读 Kafka 的 `__consumer_offsets` 或者 PG 的 replication slot 机制。

**跨领域借鉴.** 前端里的 React Fiber 调度用的 `lane` / `expiration time`、浏览器里的 `requestAnimationFrame` 批处理,本质上也是在用"单调计数器 + 事件批"替代"时间戳 + 轮询"。前后端分离的乐观锁(`If-Match: version=42`)同样是 revision 思想的投射——只要把"乐观并发控制"翻译成"我记得你当时的版本,如果你动过我就放弃",就会发现它跟 etcd 的 `WithRev` 是一套脑回路。

**未来演进的合理推断.** 当系统规模进一步扩大,单个 revision 会不够用——你需要把 revision 从 `uint64` 变成 `(term, index)` 对(这正是 Raft 日志的 log id),或者拆成 `(shard_id, local_rev)` 组合(这正是 TiKV 的 MVCC 时间戳分配方式)。PaladinCore 在 Day 4 引入 Raft 之后,其实 revision 已经悄悄绑定到了 Raft 的 `log.Index` 上,但我们还没有在数据模型里暴露这一点——这是合理的延迟决策,因为"够用"就是最好的工程美学。

## 8. 回味:记忆锚点

记忆锚点三件套:**口诀"map 不够加个号,时钟不准用计数;写多线做不到,单写入才安稳"**;**一张"事务 → 原子性 → rev 同生共死 → 单调/因果/可 Watch"的三叉树**;**决策树:先问"写是不是单线",决定用 revision 还是 CRDT**。

**下一步.** 打开 `store/bolt.go:177`,把 Delete 的 `newRev := s.rev + 1` 改成 `newRev := s.rev`,再跑 `go test ./...`——你会看到 Day 3 的三个 Watch 测试立刻挂掉。当事件流本身都保不住,拉的协议再优雅也没用——这就是下一篇引出长轮询的真正理由。

---

> **下一篇** → [02. 三级 Key 与 HTTP 语义:URL 是资源,不是参数](./02-http-tenant-api.md)
