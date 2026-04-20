# 三级 Key 与 HTTP 语义:URL 是资源,不是参数

> Day 2 · PaladinCore 源码蒸馏系列 02
>
> 把一个单机 KV 推出网络边界的时候,最容易写错的不是代码,而是**命名**。

---

## 1. 引子:扁平 key 的生死一瞬

你已经有了 Day 1 那个带 revision 的 KV 存储,下一步自然是"加个 HTTP 壳"。但真把它放到内网就会发现一个尴尬的现实:`db_host` 这个 key 在 A 服务里指 MySQL,在 B 服务里指 Redis,在 C 服务里是 PostgreSQL——扁平命名空间里三方互相践踏,任何一方改动都会牵连另外两个。你会想到加前缀 `service_a__db_host`,但这不过是把命名冲突从机器层面转移到了字符串层面,权限隔离、批量列举、命名审计统统没有答案。问题的本质不是"名字不够长",而是**配置本身是有层级的**——租户、服务、项,三件事粒度完全不同。这一篇,我们一边把 KV 暴露成 HTTP,一边把"扁平 key"拆成"三级路径",顺带把 HTTP 状态码的语义掰清楚。

## 2. 四层问题

**是什么.** 答案是把路径设计成 `/api/v1/config/{tenant}/{namespace}/{name}`,每一段对应一个维度——租户是组织边界、namespace 是服务/环境维度、name 是具体配置项。PaladinCore 在 `server/server.go:63-79` 的 `configKey` 里完成这次映射,内部 store key 则拼接成 `tenant/namespace/name`,与 BoltDB 的 B+ 树 prefix scan 天然对齐。

**怎么做.** 关键是路径段数决定操作粒度:一段视为"列 tenant 下全部",两段视为"列 namespace 下全部",三段视为"精确一项"。这种"段数即语义"的设计让同一个 handler 既能做 `GET /api/v1/config/public/` 也能做 `PUT /api/v1/config/public/prod/db_host`,不再需要单独定义列表接口。handler 入口 `server/server.go:93-123` 的 switch 正是这样做语义分发的。

**为什么.** 为什么是路径而不是 query parameter?因为 URL 路径表示**资源标识**,query 表示**过滤条件**——两者在 HTTP 缓存语义上完全不同。`/config/public/prod/db_host` 是一个可被 CDN、反向代理、浏览器 bookmark 的稳定资源;`?tenant=public&namespace=prod&name=db_host` 则是一个要逐次解析参数的查询,缓存 key 的归一化非常脆弱。此外路径还能复用 K8S API 那套成熟的资源模型,面试时一句"参考了 K8S 资源路径"就足以解释设计理由。

**放弃了什么.** 三级结构的代价是"只支持三级"。真实世界可能需要 region/cluster/tenant/namespace/name 这种五级甚至更多,PaladinCore 目前的 split 固定在三段,要往里加维度就得改 `configKey`。这是一个清晰的、有边界的决定:换来的是简单性与 K8S 风格对齐,代价是极端多租户场景下需要扩展。

## 3. 类比

**生活类比.** 这套命名就像公司邮件地址 `alice@engineering.acme.com`——`acme.com` 是租户(公司),`engineering` 是 namespace(部门),`alice` 才是具体的收件人。你不会把三级合成一个 `alice_engineering_acme`,因为第一层决定了路由(不同公司的邮件走不同服务器)、第二层决定了权限(部门内可见),第三层才是真正的终点。

**专业类比.** 这就是 K8S 的资源模型:`/api/v1/namespaces/{ns}/pods/{name}`——namespace 隔离、resource 定位、name 选中。PaladinCore 只是把 `pods` 换成了"配置项"。理解 K8S apiserver 的路径哲学,就等于理解了 PaladinCore 的 HTTP 设计。

**反类比.** 这套命名**不是**文件系统路径。虽然长得像 `/a/b/c`,但它不支持 `..`、没有符号链接、没有 chmod。如果你带着"目录"心智模型来读这段代码,就会去问"能不能 mv",答案是不行——配置本质上是扁平存储,三级只是**逻辑分层**。

## 4. 揭秘

**数据结构.** `Server` 结构(`server/server.go:22-26`)只持有 `store.Store` 和 `http.ServeMux` 两个字段——HTTP 层不拥有业务逻辑,只做路由和编解码。这是一个有意的瘦设计:换存储实现不需要动 server,换 HTTP 框架不需要动 store。

**核心逻辑:路径 → 段数 → 语义.** 路径解析最干净的部分是下面这段:

```server/server.go:63-79
func configKey(path string) (tenant, namespace, name string, err error) {
    trimmed := strings.TrimPrefix(path, "/api/v1/config/")
    trimmed = strings.TrimSuffix(trimmed, "/")

    parts := strings.SplitN(trimmed, "/", 3)
    switch len(parts) {
    case 1: return parts[0], "", "", nil              // 只有 tenant -> 列举
    case 2: return parts[0], parts[1], "", nil         // tenant+ns -> 列举
    case 3: return parts[0], parts[1], parts[2], nil   // 三段 -> 具体 key
    default:
        return "", "", "", fmt.Errorf("invalid path: %s", path)
    }
}
```

**Put 语义里的状态码细节.** 真正值得反复看的不是 handler 的主干,而是 `server/server.go:157-162` 的三行:

```server/server.go:157-162
status := http.StatusOK
if result.PrevEntry == nil {
    status = http.StatusCreated
}
w.Header().Set("X-Paladin-Revision", fmt.Sprintf("%d", result.Entry.Revision))
```

为什么要区分 200 和 201?因为 HTTP 把"幂等"和"首次创建"区分开来是写得进 RFC 7231 的约定:`201 Created` 表示这次请求产生了一个新资源,`200 OK` 表示覆盖了已有资源。部署脚本看到 201 可以触发"通知运维审查",看到 200 可以直接接受——语义靠状态码而不是靠 body。另一行 `X-Paladin-Revision` 更关键:它是 Day 3 长轮询客户端的**起点锚**,客户端下一轮 Watch 会带上这个值,服务端从这个 revision 之后返回事件。如果响应头里没有它,客户端必须再发一次 `GET /api/v1/rev`——一次多余的 RTT。

## 5. Experience

**MRE.** 启动服务 + 三类操作一条龙验证:

```bash
go test ./server/ -v            # 6/6 PASS
go run ./cmd/paladin-core serve :8080 &

curl -i -X PUT http://localhost:8080/api/v1/config/public/prod/db_host -d '10.0.0.1'
# HTTP/1.1 201 Created   <- 首次创建
# X-Paladin-Revision: 1

curl -i -X PUT http://localhost:8080/api/v1/config/public/prod/db_host -d '10.0.0.2'
# HTTP/1.1 200 OK        <- 覆盖
# X-Paladin-Revision: 2

curl http://localhost:8080/api/v1/config/public/prod/
# 列出 namespace 下所有 key
```

**参数旋钮:段数 vs 动作.** 把 URL 从三段缩成两段或一段,同一个 handler 会走不同分支。`GET /api/v1/config/public/` 列出 tenant 下全部、`GET /api/v1/config/public/prod/` 列出 namespace 下全部、`GET /api/v1/config/public/prod/db_host` 返回单项;而 `PUT` 则要求必须三段,否则返回 400 "name is required for PUT"(`server/server.go:108-113`)。这是"段数即语义"的直接观察路径。

**极端场景:1 MB body 上限.** `server/server.go:144` 用 `io.LimitReader(r.Body, 1<<20)` 把 body 截到 1 MB。试一下 `dd if=/dev/zero bs=1M count=2 | curl -X PUT --data-binary @- ...`,服务端只会读前 1 MB——再长的配置值被截断,防止内存 DoS。

**Benchmark 取向.** `ab -n 10000 -c 32 http://localhost:8080/api/v1/rev` 对着只读的 `/api/v1/rev` 跑,单机可到几万 QPS;PUT 受 BoltDB fsync 限制在 1-5k/s。读写差了一个数量级,这是 Day 5 "读走本地,写走 Raft" 拆开的早期依据。

## 6. Knowledge

**一句话本质.** 把扁平 KV 升级为配置中心,靠的是"路径即资源 + 段数即语义 + 状态码即契约"这三件事。

**决策树.**

```
需要给外部客户端暴露吗?
├─ 否 → CLI/gRPC 内部工具就够,不必套 HTTP
└─ 是 → 配置是否天然分层(租户/环境/服务)?
        ├─ 否 → 扁平 key + 前缀约定
        └─ 是 → 三段(或 N 段)路径,每段对应一个维度
                ├─ 有幂等语义? -> 用 PUT,区分 200/201
                └─ 有流式/推送? -> Day 3 的 Watch,延用同一路径哲学
```

**知识图谱.**

```
URL 路径 ──分段映射──→ store key
   │                     │
   ↓                     ↓
段数 → 语义分发     prefix scan (B+ tree)
   │                     │
   ↓                     ↓
CRUD 四路由         List 一次遍历
         ↘         ↙
       统一 ConfigResponse 结构
       (带 Revision,供 Watch 锚点)
```

## 7. Transfer

**相似场景.** 任何"资源 + 操作"的 API 都可以套这套路径哲学:K8S apiserver、GitHub REST v3 的 `/repos/{owner}/{name}`、阿里云 OpenAPI 的 `region/project/resource` 三段 ARN,本质都是"层级命名空间 + 标识符"的组合。

**跨域借鉴.** 前端的 React Router 嵌套路由(`/teams/:team/projects/:project/tasks/:task`)是同一模式在 UI 域的投射——段数决定展示的粒度,从而可以让同一组件树按层级加载。

**未来演进.** 当租户数量从千级扩到百万级,`/api/v1/config/{tenant}/...` 的单体服务会扛不住,自然会演化为按 tenant hash 分片的 Gateway + Backend 两层结构——tenant 一旦是路径第一段,分片就是免费的。这也是为什么把"租户放在第一段"比"放在 query"更未来友好。

## 8. 回味

记忆锚点三件套:**口诀"路径是资源,query 是过滤;段数定语义,201 分创建"**;**一张"URL → 段数 → handler 分支 → store key"的流向图**;**一棵"需不需要暴露 / 要不要分层 / 要不要幂等"的决策树**。

**下一步你可以做的.** 把 `server/server.go:157-160` 改成"永远返回 200",运行 `server_test.go` 会看到 `TestPutCreatesAndUpdates` 之类的测试立刻挂掉——这就是"语义靠状态码"的负面实证。

---

> **上一篇** ← [01. Revision:用一个递增计数器当逻辑时钟](./01-revision-kv-store.md)
> **下一篇** → [03. 环形缓冲 + sync.Cond:把轮询压成 RTT](./03-watch-ring-buffer.md)
