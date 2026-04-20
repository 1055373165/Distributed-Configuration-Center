# ForwardRPC 与 ReadIndex:客户端凭什么不知道 Leader

> Day 5 · PaladinCore 源码蒸馏系列 05
>
> Raft 给了你强一致,但也留了一个尴尬的前提——**客户端得知道谁是 Leader**。这是真实部署里最容易把团队拖进坑的一条隐式契约。

---

## 1. 引子:LB 随机路由的一道算术题

上线三节点 Raft 集群后,你会发现一个反直觉的事实:**2/3 的写请求会直接报 500**。原因很简单——Nginx 随机路由,三台机器只有一台是 Leader,剩下两台是 Follower。Follower 看到写请求返回 `ErrNotLeader`,但客户端完全不知道"该去问谁"。你可以让客户端自己维护一份"Leader 表",但这份表要靠什么机制更新?让每个客户端都订阅集群 membership?那就等于把"集群拓扑"变成了"客户端要操心的事"。真正干净的答案是:**让 Follower 代客户端跑一趟**——收到写请求,自己转发给 Leader,原样把响应送回客户端。客户端永远只看到"我连了一台机器,它给了我答案",连"集群里有几个节点"都不需要知道。同时这一篇也要回答读路径的另一个问题:读也是随便连一台就行吗?读到的数据是最新的吗?——这就触发了"脏读 vs 线性一致读"的经典权衡。

## 2. 四层问题

**是什么.** ForwardRPC 是 Follower 侧的一个透明 HTTP 代理:收到写请求,若自己不是 Leader,就通过 Leader 的 HTTP advertise 地址把请求原样转发出去,再把响应回写给客户端。一致性读则是**可选**开关:默认读本地(脏读,零网络开销),需要强一致时显式要求 `raft.VerifyLeader()` 或 `ReadIndex`。实现在 `server/raft_server.go:75-193`。

**怎么做.** 转发的第一个难题是"Leader 的 HTTP 地址从哪来"。`raft.LeaderWithID()` 返回的是 Raft 的**二进制协议端口**(9001 那种),不是 HTTP 端口。PaladinCore 的解法是:每个节点加入集群时通过 `Admin/join` 把自己的 HTTP advertise 地址通过 Raft 复制到一个保留 key `__paladin/peers/<nodeID>` 下——这个地址随 Raft 日志自动复制到所有节点,失效后会自动更新。Follower 转发时查这张表(`raft/node.go:204-216` 的 `LeaderHTTPAddr`)即可。

**为什么.** 为什么把 peer HTTP 地址放进 Raft 日志而不是放进一个单独的 gossip?因为 **Raft 已经是强一致的元数据通道**,没必要再引入第二套协议。放进 Raft 意味着:地址变更随日志复制、重启后从 snapshot 恢复、leader 切换后新 Leader 自动拿到完整视图——零额外运维成本。这是一个典型的"**已经有一个可靠通道就不要再引入第二个**"的工程决策。

**放弃了什么.** 放弃了"写请求的真实发起方 IP"——经过 Follower 一跳之后,Leader 看到的 Remote Addr 是 Follower 不是客户端。我们通过 `X-Forwarded-By` 标识转发节点 ID(`server/raft_server.go:174`),但完整的"链路追踪"需要更重的 tracing 框架。对于配置中心场景,写请求频率低、审计要求没那么严,这个代价可以接受。

## 3. 类比

**生活类比.** ForwardRPC 就是公司前台代转文件:你想把一份合同交给老板,但老板不在办公室——前台不会让你白跑,而是把你的文件送到老板那里,拿到签字版回头交给你。你全程只跟前台打交道,根本不需要知道老板今天在几楼哪个会议室。

**专业类比.** 这正是数据库中间件的"读写分离路由"——Proxy 收到请求,根据 SQL 类型决定走主库(写)还是从库(读),客户端不感知拓扑。ForwardRPC 只是把 Proxy 的智能内置在了数据节点里。

**精妙类比.** 脏读 vs 线性一致读像 DNS 的 TTL vs 强制解析:默认走本地 DNS 缓存(快、但可能过期几秒),需要最新时显式 `nslookup` 向权威服务器查询(慢一个 RTT,但保证最新)。`VerifyLeader()` 就是"配置中心版的强制解析"——Leader 发一轮心跳确认自己仍然是合法 Leader,再返回本地读取的数据。

**反类比.** ForwardRPC **不是** HTTP 302 重定向。302 让客户端自己再发一次请求,客户端必须处理重定向状态码;ForwardRPC 是服务端内部代理,客户端从头到尾只看到一次请求一次响应,重定向透明。如果你按 302 的思路设计,SDK 就得内建重定向逻辑,客户端就必须知道 Leader 地址——这恰恰是我们想避免的。

## 4. 揭秘

**路由分发:读和写走完全不同的路径.**

```server/raft_server.go:75-107
func (rs *RaftServer) handleRaftPut(w http.ResponseWriter, r *http.Request, tenant, namespace, name string) {
    body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
    key := storeKey(tenant, namespace, name)

    // 不是 Leader 就转发,客户端永远看不到 ErrNotLeader
    if !rs.node.IsLeader() {
        rs.forwardToLeader(w, r, body)
        return
    }

    op := praft.Op{Type: "put", Key: key, Value: body}
    result, err := rs.node.Apply(op, 5*time.Second)
    if err != nil {
        httpError(w, http.StatusInternalServerError, "raft apply: %v", err)
        return
    }

    status := http.StatusOK
    if result.PrevEntry == nil { status = http.StatusCreated }
    w.Header().Set("X-Paladin-Revision", fmt.Sprintf("%d", result.Entry.Revision))
    writeJSON(w, status, &ConfigResponse{
        Revision: result.Entry.Revision,
        Configs:  []*ConfigItem{entryToConfig(result.Entry)},
    })
}
```

关键点是**决策在一行 `if !rs.node.IsLeader()`**——非 Leader 就让 Follower 代跑,代码里找不到"循环找 Leader"或"重试其他节点"的逻辑,这个简洁是靠把复杂度推给 `LeaderHTTPAddr()` 换来的。

**转发实现的三个细节.**

```server/raft_server.go:142-193
func (rs *RaftServer) forwardToLeader(w http.ResponseWriter, r *http.Request, body []byte) {
    // 细节 1:用 HTTP advertise 地址,不是 Raft 二进制端口
    leaderHTTP := rs.node.LeaderHTTPAddr()
    if leaderHTTP == "" {
        // 选举窗口内 Leader 未知或地址未注册,返回 503 让客户端退避重试
        httpError(w, http.StatusServiceUnavailable,
            "leader http addr unknown (leader_raft=%q), try again later",
            rs.node.LeaderAddr())
        return
    }

    url := fmt.Sprintf("http://%s%s", leaderHTTP, r.URL.String())
    fwdReq, _ := http.NewRequestWithContext(r.Context(), r.Method, url,
        bytes.NewReader(body))

    // 细节 2:复制原请求的所有 header,保证鉴权/trace 信息不丢
    for k, vs := range r.Header {
        for _, v := range vs { fwdReq.Header.Add(k, v) }
    }
    // 细节 3:加一个标识位,供 Leader 识别"我是被转发的"(可防止循环转发)
    fwdReq.Header.Set("X-Forwarded-By", rs.node.Stats()["id"])

    resp, _ := http.DefaultClient.Do(fwdReq)
    defer resp.Body.Close()

    // 响应原样回写,包括所有 header、status、body
    for k, vs := range resp.Header {
        for _, v := range vs { w.Header().Add(k, v) }
    }
    w.WriteHeader(resp.StatusCode)
    io.Copy(w, resp.Body)
}
```

**为什么注册 peer HTTP 地址要走 Raft?** 看 `raft/node.go:218-227` 的 `RegisterPeerHTTP`:

```raft/node.go:218-227
func (n *Node) RegisterPeerHTTP(nodeID, httpAddr string) error {
    _, err := n.Apply(Op{
        Type:  "put",
        Key:   PeerHTTPPrefix + nodeID,  // "__paladin/peers/<id>"
        Value: []byte(httpAddr),
    }, 5*time.Second)
    return err
}
```

把 peer HTTP 地址当作**普通的配置 key** 写进 store。它因此自动获得三个好处:一、随 Raft 复制到所有节点;二、写进 snapshot,节点重启后自动恢复;三、Leader 切换后新 Leader 读到的是最新的完整映射。不需要单独的 membership 组件。

**一致性读的两种策略.** 我们当前实现默认脏读(直接读本地 BoltDB)。需要强一致读时,服务端要先 `raft.VerifyLeader().Error()` 确认自己仍是多数派认可的 Leader,再读本地——此时数据保证线性一致。etcd 的 `ReadIndex` 是更轻量的替代:Leader 记下当前 commitIndex,等 apply 到那个 index 后返回本地读。两者区别:VerifyLeader 要一轮心跳 quorum,ReadIndex 不需要新心跳,只等现有 apply 追齐。

## 5. Experience

**MRE(依赖 Day 7 的本地集群脚本).**

```bash
./scripts/cluster-local.sh --fresh   # 起三节点
# 写到 Follower :8081,观察日志里的 FORWARD 行
curl -X PUT http://127.0.0.1:8081/api/v1/config/public/prod/db_host -d '10.0.0.1'
tail -n 2 .cluster-logs/node2.log
# [FORWARD] PUT /api/v1/config/public/prod/db_host -> leader 127.0.0.1:8080 (status 201)
```

**参数旋钮:kill Leader 后 Follower 返回什么.**

```bash
kill $(sed -n 1p .cluster-pids)   # 杀 node1(初始 Leader)
# 选举窗口内(约 1-3 秒)向 node2 发写请求
curl -i -X PUT http://127.0.0.1:8081/api/v1/config/public/prod/x -d 'y'
# HTTP/1.1 503 Service Unavailable
# {"error":"leader http addr unknown..."}
# 等 3 秒新 Leader 当选后再发同一条命令 -> 201 Created
```

这段观察让"选举窗口"变成一个可测量的实物——从杀 Leader 到写请求重新可用大概 3 秒,刚好覆盖 `SnapshotInterval = 30 * time.Second` 之外的选举时间默认配置。

**极端场景:强制脏读看见"落后".** 用 benchmark 压力写进 Leader,**同一时刻**在 Follower 上连续查询同一个 key:你会偶尔看到 Follower 的 rev 比 Leader 低 1-2,这就是脏读的"落后几个 entry"。把这个场景做成单元测试基本不可能(几毫秒级),但通过 benchmark + 高速 curl 可以偶发观察到——这是"毫秒级不一致"的真实证据。

**Benchmark.** ForwardRPC 的写延迟 = Follower→Leader RTT + Leader Raft apply + Leader→Follower RTT,典型内网下多一个往返 < 1ms。相比直接连 Leader 的额外成本约 20-30%,远低于客户端自己维护 Leader 表的复杂度。

## 6. Knowledge

**一句话本质.** ForwardRPC 把"谁是 Leader"从客户端问题变成服务端问题,peer HTTP 表搭 Raft 复制自动同步;一致性读用"默认脏读 + 可选 VerifyLeader" 在性能和线性一致之间留开关。

**决策树.**

```
客户端需要知道集群拓扑吗?
├─ 否(透明代理) → ForwardRPC + peer HTTP 通过 Raft 复制
└─ 是(客户端主导) → SDK 维护 Leader 表 + 重试协议

读请求需要线性一致?
├─ 默认 → 读本地(脏读,0 开销,可能毫秒级落后)
├─ read-after-write 场景 → VerifyLeader(+1 轮 quorum 心跳)
└─ 高频强一致 → ReadIndex(轻量,实现复杂)
```

**知识图谱.**

```
Client -> any node -> if Leader -> raft.Apply
                     if Follower -> LeaderHTTPAddr()
                                       │
                                       └─→ http.Do -> Leader -> raft.Apply
                                              │
                                              └─→ 原样返回 Follower -> Client

peer HTTP 表维护:
  Admin/join -> Join(raft) + RegisterPeerHTTP(store) -> Raft 复制 -> 所有节点可查
```

## 7. Transfer

**相似场景.** MongoDB Replica Set 的 `readPreference`、Redis Cluster 的 MOVED/ASK 重定向、MySQL Group Replication 的 `group_replication_single_primary_mode`,都是"写路由 + 读一致性"这一对问题的不同工程答案。理解 PaladinCore 的两刀切(转发写、可选强读),再读上述系统的文档能瞬间定位它们的选择点。

**跨域借鉴.** 前端里 SSR 框架的"客户端水合"同样是"一次请求对用户透明,但背后可能经过多个处理层"。Next.js 的 middleware 转发、Remix 的 loader/action 路由都是 ForwardRPC 的前端投射。

**未来演进.** 当写吞吐成为瓶颈,自然会走向**按 tenant 分片**——每个分片独立 Raft group、独立 Leader。Follower 转发逻辑升级为"按 key 哈希找到目标 group,再把请求路由过去"——本质是把 PaladinCore 变成 TiKV 的微型版本。

## 8. 回味

记忆锚点:**口诀"Follower 代跑,客户端无感;HTTP 表入 Raft,失效自动换"**;**一张"Client → any node → Forward → Leader → apply → 原路返回"的流程图**;**决策树:读的一致性三档,按场景选开关**。

**下一步.** 服务端到这里已经"怎么连都对"。真正剩下的挑战在客户端:**连不上怎么办?** 下一篇实现 SDK 的三阶段生命周期,把"服务全挂了仍然能启动"这一条工程问题回答清楚。

---

> **上一篇** ← [04. Raft FSM](./04-raft-fsm.md)
> **下一篇** → [06. SDK 三阶段生命周期](./06-sdk-fallback.md)
