# 环形缓冲 + sync.Cond:把轮询压成 RTT

> Day 3 · PaladinCore 源码蒸馏系列 03
>
> 把"客户端反复来问"变成"服务端变化时通知",需要的不是更快的网络,而是更聪明的数据结构和更耐心的 goroutine。

---

## 1. 引子:一个足够荒谬的反例

假设 5000 个 Pod 每秒各发一次 `GET /api/v1/config/.../db_host`,而 db_host 一天只改两次。你会看到一幅这样的图景:服务端每秒处理 5000 次"没变化"的查询,换来的是每天 2 次有效的配置推送——**命中率万分之零点四**。即使每次查询只花 1 ms CPU,每台机器也等于在白烧 5 核算力。换一个方向思考:如果客户端可以说"我等最多 30 秒,这段时间里有变化就立刻告诉我",那同样的 5000 个 Pod,只需要有变化时服务端把事件投递一次——CPU 占用接近 0,延迟从"轮询间隔"降到"网络 RTT"。这就是长轮询,它的服务端实现需要解决两个经典问题:**怎么存最近的事件供按 revision 查询**,和**怎么让等待的 goroutine 既不忙等又能被及时唤醒**。

## 2. 四层问题

**是什么.** Watch 的内核是一个固定容量的环形缓冲区加一个 `sync.Cond`。每次 `Put`/`Delete` 都会把一个 `Event` 追加到环形缓冲里并 `Broadcast()` 唤醒所有长轮询的 goroutine;等待的 goroutine 醒来后检查"有没有我想要的 revision > N 的事件",有就返回,没有就继续等待或超时。定义在 `store/watch.go:42-49`。

**怎么做.** 追加是 `pos := count % capacity; buf[pos] = ev; count++`(`store/watch.go:62-72`),`count` 只增不减,所以根据 `count` 和 `capacity` 永远能算出"当前有效事件的最老位置"。查询时从 `oldest = max(0, count - capacity)` 遍历到 `count-1`,跳过 `revision <= afterRev` 的和不匹配 prefix 的(`store/watch.go:126-151`)。

**为什么.** 为什么是环形缓冲而不是 slice、链表或 channel?slice/链表会随事件增长无上限(长连接系统里是内存炸弹的正步走),channel 是纯 FIFO 不支持"按 revision 查询"——你没法从一个 channel 里查"rev > 42 的事件",除非把所有事件消费一遍。环形缓冲牺牲"历史完整性"(老事件会被覆盖)换来"内存恒定",这对配置中心是**正确的取舍**:配置的最终态保持一致比"审计每一次变更"重要。

**放弃了什么.** 放弃了"事件永不丢失"。如果客户端离线太久,它上次的 revision 已经从环形缓冲里被挤出去,下次回来会拿不到"中间变更"的事件。解决方式是服务端返回 `ErrCompacted` 让客户端触发全量重拉(etcd 的做法),也可以像我们简化版一样"能给多少给多少"。两种都能保证"最终态正确",差别只在"中间轨迹是否可见"。

## 3. 类比

**生活类比.** 环形缓冲像办公室门口的公告板,只贴最近 N 张通知,新通知覆盖最老的。路过的人一看就能知道"最近发生过什么",但如果你出差两个月回来,最早那张通知已经被覆盖——你只能去 HR 处全量补课。

**专业类比.** 这就是 Linux 内核的 `dmesg` ring buffer、Kafka 的 topic retention、CPU 的 trace buffer。都是同一思路:用固定内存换低延迟的近态查询,牺牲远期完整性。

**精妙类比.** `sync.Cond` 本质上是条件变量的"生产者-消费者协议":消费者持锁检查条件,不满足就 `Wait` 释放锁 + 挂起;生产者持锁改状态,改完 `Broadcast` 把所有挂起者唤醒;被唤醒的消费者重新拿锁、重新检查条件——**这正是操作系统课里的 monitor 模式**。

**反类比.** `sync.Cond` 不是 channel。channel 是"传值",Cond 是"传信号";channel 的 select 天然支持超时,Cond 没有超时需要自己用 `time.AfterFunc` 模拟(`store/watch.go:105-117`);channel 一次 recv 消费一个值,`Broadcast` 一次唤醒所有等待者。误以为"Cond 就是带信号的 channel"会让你写出死锁的代码。

## 4. 揭秘

**数据结构.** `WatchCache` 只有五个字段,三个是骨架:`mu`/`cond` 是同步原语,`buf` 是固定容量的事件数组,`capacity` 是容量,`count` 是**总追加次数**(不是当前大小)。`count` 单调递增的性质是整个算法的核心不变量。

**两个看似矛盾的事实:Append 不阻塞,WaitForEvents 却可能阻塞很久.** 原因是它们共享同一把 `mu`,但用法完全不同。Append 持锁很短:写一个数组格子 + 递增计数器 + `Broadcast`——全是 O(1) 操作。WaitForEvents 在持锁状态下调用 `cond.Wait()`,`Wait` 的语义是**原子地释放锁并挂起 goroutine**,所以 Append 在 WaitForEvents 阻塞期间可以照常拿到锁。

**核心逻辑:长轮询三步.**

```store/watch.go:83-122
func (wc *WatchCache) WaitForEvents(afterRev uint64, prefix string, timeout time.Duration) []Event {
    deadline := time.Now().Add(timeout)
    wc.mu.Lock()
    defer wc.mu.Unlock()

    for {
        if wc.closed { return nil }

        // 第 1 步:持锁检查,有就返回
        events := wc.getEventsLocked(afterRev, prefix)
        if len(events) > 0 { return events }

        // 第 2 步:超时直接退出
        remaining := time.Until(deadline)
        if remaining <= 0 { return nil }

        // 第 3 步:没事件且没超时,挂起等待。
        // sync.Cond 没有原生 timeout,用一个 timer goroutine 模拟:
        // 到期 Broadcast,Wait 就会被唤醒重新进入 for 循环
        done := make(chan struct{})
        go func() {
            timer := time.NewTimer(remaining)
            defer timer.Stop()
            select {
            case <-timer.C:  wc.cond.Broadcast()
            case <-done:
            }
        }()
        wc.cond.Wait()
        close(done)
    }
}
```

**为什么必须 `for` 循环包 `Wait`?** 这是 `sync.Cond` 写作的铁律。`Broadcast` 会唤醒**所有**等待者,但某个 goroutine 醒来时,它等的事件(特定 prefix)可能并不在最新那批里;此外还有虚假唤醒可能。唯一正确的写法是"被唤醒后重新检查条件",不满足继续 `Wait`——这正是上面 for 循环的语义。

**WatchableStore 的粘合层.** 事件是怎么从"写操作"冒到"环形缓冲"里的?`store/watchable.go:30-43` 的 `Put` 先调底层 BoltStore,再 `wc.Append(Event{...})`;`Delete` 同理。这里用 Go 的 embedding (`*BoltStore`) 继承了全部读方法,只重写 `Put`/`Delete` 两个写操作——这是"组合优于继承"在 Go 里的典范用法。

## 5. Experience

**MRE.** 三窗口复现长轮询的**亚毫秒级**延迟:

```bash
# Terminal 1
go run ./cmd/paladin-core serve :8080

# Terminal 2:阻塞等待最多 30 秒
time curl "http://localhost:8080/api/v1/watch/public/prod/?revision=0&timeout=30"

# Terminal 3:任意时刻触发一次 PUT
curl -X PUT http://localhost:8080/api/v1/config/public/prod/db_host -d '10.0.0.1'
```

Terminal 2 的 `time` 输出会显示**几毫秒**——从"PUT 进入服务端"到"长轮询返回"的实际延迟就是内网 RTT + goroutine 唤醒时间。

**参数旋钮:容量 vs 延迟.** 默认 `defaultWatchCacheSize = 4096`(`store/watchable.go:13`)。把它调到 16,用 benchmark 高速写 1000 条数据,再用一个"上次离线"的客户端去 Watch 历史 revision——它会拿不到最老的几十条事件。这就是环形溢出在真实场景的体现。

**极端场景 1:超时后再写,客户端下一轮拿得到.** 发起一个 `?timeout=2` 的 Watch,两秒内不写,返回空事件;接着你写一条,下一轮 Watch(revision 还停在旧值)仍能拿到。原因在于 `count` 永远单调递增,超时仅让这一次请求"空手而归",不会抹掉状态。

**极端场景 2:为什么 Wait 必须在 for 里.** 把 `store/watch.go:119` 的 `wc.cond.Wait()` 单独放在 if 里,去掉外层 for,运行 `TestWatchCachePrefixFilter` 会偶现失败——因为另一 goroutine 写入的事件 prefix 不匹配时,当前 goroutine 被"假唤醒",直接返回空。这是 `sync.Cond` 坑位的真实重现。

**Benchmark.** `go test -bench=. ./store/ -run=^$` 可见 Append 在 10M ops/s 量级,WaitForEvents 的瓶颈不在缓冲而在网络。用 4096 条 cap 的缓冲,单机支撑数千 Watch 客户端完全没压力。

## 6. Knowledge

**一句话本质.** 长轮询是"用一次 TCP 长连接 + 一次条件变量等待,把 N 次无效轮询压缩成一次有效推送"。

**决策树.**

```
需要客户端感知变更?
├─ 否(纯读) → 直接 GET,不需要 Watch
└─ 是 → 中间代理是 HTTP 还是 HTTP/2?
        ├─ 纯 HTTP/1.1 + 老 LB → 长轮询(穿透最友好)
        ├─ 全链路 HTTP/2 → gRPC 双向流(连接复用更省)
        └─ 浏览器内 → SSE / WebSocket
```

**知识图谱.**

```
BoltStore.Put ──→ WatchableStore.Put ──→ WatchCache.Append
                                            │
                                            ├─ buf[pos]=ev
                                            ├─ count++
                                            └─ cond.Broadcast
                                                    │
                                                    ↓
               WaitForEvents ←─── goroutine (持锁 Wait)
                     │
                     ↓
               getEventsLocked (oldest..count-1)
```

## 7. Transfer

**相似场景.** K8S apiserver 的 Watch、etcd 的 Watch RPC、Nacos 的长轮询推送,都是"变更事件 + 按版本过滤 + 条件变量唤醒"的同一个组合。Kafka consumer 的 `fetch.max.wait.ms` 本质上是在 broker 侧做长轮询。

**跨域借鉴.** 浏览器里的 `IntersectionObserver` / `ResizeObserver` 是"从拉变推"的前端版本——宿主主动告诉你"有变化了",组件只在变化时重渲染。理解长轮询可以帮你理解为什么 Observer API 能极大减少 requestAnimationFrame 的使用。

**未来演进.** 当 Watch 客户端从千级到百万级,单个 sync.Cond + 单个 mu 会成为锁竞争点。etcd 的应对是**按 key range 分片 WatchableStore**,每个分片独立锁;更进一步是把 Watch 协议换成 gRPC 双向流 + server-side 过滤,减少无效事件广播。

## 8. 回味

记忆锚点:**口诀"环形存近态,Cond 省 CPU;for 循环保虚唤,count 永不退"**;**一张"Put → Append → Broadcast → Wait 被唤醒 → 重查"的时序图**;**决策树:看代理类型定协议(长轮询/gRPC 流/SSE)**。

**下一步.** 把 `defaultWatchCacheSize` 改成 4,跑 `TestWatchCacheRingOverflow`,你会亲眼看到"最老事件被覆盖"——这是环形缓冲最直观的断面。更重要的是:单机这一层已经做到极致,下一步要回答"**单点挂了怎么办**"——引入 Raft。

---

> **上一篇** ← [02. 三级 Key 与 HTTP 语义](./02-http-tenant-api.md)
> **下一篇** → [04. Raft FSM:把写操作从单机调用抽象为日志事件](./04-raft-fsm.md)
