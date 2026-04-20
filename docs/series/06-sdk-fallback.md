# SDK 三阶段生命周期:启动 / 运行 / 关机,缺一不可

> Day 6 · PaladinCore 源码蒸馏系列 06
>
> 服务端做得再漂亮,一个设计糟糕的 SDK 依然会把整套系统拖进 4 点钟的值班告警。

---

## 1. 引子:凌晨四点不能响的告警

想象一下:凌晨四点,配置中心集群因为网络分区暂时不可达 30 秒。这时候上线的 200 个新 Pod 怎么办?如果 SDK 的设计是"启动必须连上服务端,否则 panic",那就是 200 个 Pod 同时启动失败,值班工程师被电话轰炸起床。但如果 SDK 有**本地缓存降级**——即使连不上服务端也能用昨晚的配置继续启动,等服务端回来之后自动追赶到最新版——凌晨四点就什么都不会发生,告警静悄悄。这一篇要回答的核心问题是:一个生产级 SDK 客户端的基本形状是什么?答案不是"一个 HTTP 客户端",而是**启动 + 运行 + 关机**三段式生命周期,每一段都有独立的失败语义。

## 2. 四层问题

**是什么.** SDK 的生命周期分三段:Startup 做全量拉取(失败就走本地缓存),Runtime 用一个后台 goroutine 跑长轮询接收增量变更,Shutdown 通过 `context.Cancel()` 优雅停止轮询。全部实现在 `sdk/client.go:70-124`。

**怎么做.** 关键在三件事的**失败路径**:startup 的 `fullPull()` 失败不能让 `New()` 返回 error,得尝试 `loadFromCache()` 兜底;runtime 的长轮询出错不能让 goroutine 崩溃,得指数退避重试;shutdown 的 `Close()` 必须保证后台 goroutine 彻底退出,靠 `context.WithCancel` + `sync.WaitGroup` 组合实现。

**为什么.** 为什么本地缓存还要 SHA-256 校验?因为进程可能在写缓存的中途被 kill,磁盘可能出位翻转——没有校验就意味着 SDK 可能加载**半损坏的配置**启动,后果比"启动失败"更严重(静默的错误配置可能导致线上事故几小时都看不出来)。校验失败直接拒绝加载,要么成功 fullPull 要么空配置启动,**没有"带坏配置跑"这个选项**。

**放弃了什么.** 放弃了"本地缓存永远是最新的保证"。如果 SDK 最后一次成功拉取是一周前,中间服务端挂了又恢复但 SDK 从没回来过(极端情况),缓存里可能是过期配置。解决方式是显式声明"关键配置需 fresh"——比如限流阈值之类,允许 SDK API 提供 `RequireFresh(key)` 让用户主动阻断启动。当前实现把这个决策留给用户场景。

## 3. 类比

**生活类比.** 三阶段生命周期就是出差住酒店:Check-in 是 Startup(办入住、拿钥匙、找房间),住的这几天是 Runtime(每天进进出出,房卡随时刷),Check-out 是 Shutdown(结账、交钥匙、退房)。每一段都有自己的失败处理:Check-in 失败可以去备用酒店(本地缓存),Runtime 中钥匙失灵去前台重做(重试),Check-out 时没付账酒店会挂账(Close 不彻底会泄露)。

**专业类比.** 这正是操作系统进程模型的 `fork/exec` + `wait4`:父进程启动子进程(startup),子进程在 event loop 里处理信号(runtime),父进程通过 SIGTERM + waitpid 清理(shutdown)。Go 的 `context.Cancel()` + `sync.WaitGroup` 是这个模式在 userspace 的缩小版。

**精妙类比.** SHA-256 校验缓存像"火车票二维码"——二维码本身带防伪,即使有人伪造一张图,扫码时校验失败就进不了站。缓存文件被意外截断或磁盘翻位就是"伪造",checksum 就是那个防伪码。

**反类比.** 长轮询循环**不是**无脑死循环。一个新手容易写出 `for { c.client.Get(...); }` 这种"出错就重连"的代码,实际它在服务端短暂不可用时会把 CPU 烧到 100%。PaladinCore 的 `sdk/client.go:172-175` 的 `sleep(c.config.RetryBackoff)` 是必不可少的阀门——错了就等一下,别用死循环打服务端屁股。

## 4. 揭秘

**数据结构.** `Client` 结构(`sdk/client.go:35-45`)有九个字段,其中四个和生命周期直接相关:`ctx`/`cancel` 驱动 goroutine 退出,`wg` 等待 goroutine 真正结束,`client` 是 HTTP 客户端(特殊配置了 `Timeout: PollTimeout + 5s`——客户端侧超时**必须比服务端侧长**,否则每次都是客户端先 hang 住再重连,服务端长轮询白等)。

**Startup 的三路兜底.**

```sdk/client.go:86-94
if err := c.fullPull(); err != nil {
    log.Printf("[SDK] full pull failed: %v, trying cache", err)
    if cacheErr := c.loadFromCache(); cacheErr != nil {
        log.Printf("[SDK] cache load failed: %v", cacheErr)
    }
}
c.wg.Add(1)
go c.watchLoop()
return c, nil
```

这段代码最值得细读的是"**两个错都吃掉,New 仍然返回 nil**"。理由是:**让 SDK 启动"永远成功"**——即使 fullPull 和 cache 都失败,client 依然返回一个空的 configs map 和正在后台重试的 watchLoop。调用方的业务代码不会因 SDK 初始化而阻塞 / panic。这种"**永远可用的空状态**"是面向生产的基本态度。

**Runtime:一个 goroutine 吃掉全部复杂度.**

```sdk/client.go:156-194
func (c *Client) watchLoop() {
    defer c.wg.Done()
    for {
        // 每轮开头先检查 ctx,Close() 时立刻退出
        select {
        case <-c.ctx.Done(): return
        default:
        }

        c.mu.RLock(); rev := c.revision; c.mu.RUnlock()
        url := fmt.Sprintf("http://%s/api/v1/watch/%s/%s/?revision=%d&timeout=%d",
            c.pickAddr(), c.config.Tenant, c.config.Namespace,
            rev, int(c.config.PollTimeout.Seconds()))

        resp, err := c.client.Get(url)
        if err != nil {
            log.Printf("[SDK] watch error: %v, retry in %v", err, c.config.RetryBackoff)
            c.sleep(c.config.RetryBackoff)   // 失败退避,避免拥塞
            continue
        }

        var wr watchResponse
        json.NewDecoder(resp.Body).Decode(&wr)
        resp.Body.Close()

        if len(wr.Events) > 0 {
            c.applyEvents(wr.Events)   // 应用到内存 + 触发 OnChange
            c.saveToCache()             // 立刻持久化,下次 startup 可以用
        }
        if wr.Revision > 0 {
            c.mu.Lock(); c.revision = wr.Revision; c.mu.Unlock()
        }
    }
}
```

这一段的精华不在代码,而在**顺序**:先检查 ctx,再拉 revision,再发请求,再应用事件,再更新 revision。每一行都精确对应生命周期的一个状态转换,调整顺序就会出问题(比如"发请求前不 RLock 读 revision"可能读到正在被其他回调改的旧值)。

**SHA-256 校验把"损坏"从"错误"里独立出来.**

```sdk/client.go:264-289
func (c *Client) loadFromCache() error {
    data, err := os.ReadFile(path)
    if err != nil { return err }        // 文件不存在:正常 case,空启动
    var cf cacheFile
    if err := json.Unmarshal(data, &cf); err != nil {
        return fmt.Errorf("corrupt cache: %w", err)  // JSON 损坏:拒绝加载
    }
    cfgData, _ := json.Marshal(cf.Configs)
    if sha256Sum(cfgData) != cf.CheckSum {
        return fmt.Errorf("cache checksum mismatch")  // 内容被篡改/截断:拒绝加载
    }
    // 校验通过,写入内存
    c.mu.Lock()
    for k, v := range cf.Configs { c.configs[k] = []byte(v) }
    c.revision = cf.Revision
    c.mu.Unlock()
    return nil
}
```

三条失败路径各自返回不同的 error,允许上层区分"缓存不存在"(轻度)、"文件损坏"(中度)、"checksum 不匹配"(重度)。这是"**让错误可观测、可分类**"的细节,生产环境里这些 error 都会进 metrics。

## 5. Experience

**MRE.** 跑 SDK 自带的五个测试,覆盖四个失败路径:

```bash
go test ./sdk/ -v
# TestSDKFullPullAndGet         正常连服务器全量拉取
# TestSDKWatchUpdates            服务端 PUT -> SDK 立刻收到回调
# TestSDKCacheFallback           起服务端 -> Close -> 重新 New,走 cache
# TestSDKCacheChecksumValidation 人为损坏缓存文件 -> 拒绝加载
# TestSDKServerDown              服务端从不起来 -> New 不 panic,空配置
```

**参数旋钮:PollTimeout 与客户端超时.** 默认 `PollTimeout = 30s`,`http.Client.Timeout = PollTimeout + 5s`(`sdk/client.go:84`)。把它们改成一样的 30s,然后发起 Watch——偶尔会看到客户端"err: i/o timeout",因为服务端长轮询刚好在 30s 返回,客户端已经超时。**客户端超时必须比服务端大一点**,这是长轮询协议最容易踩的坑。

**参数旋钮:RetryBackoff 与服务端抖动.** 默认 1 秒退避。把它改成 100 毫秒,然后用 `cluster-stop.sh` 把服务端全杀掉,观察 SDK 日志——每 100ms 一行 "watch error, retry in 100ms",大概 10 条/秒。再把它改回 1s,日志降到 1 条/秒。这就是退避参数对"失败风暴"直观的调节。

**极端场景:缓存被截断一半.**

```bash
# 先让 SDK 成功跑一次生成缓存
go test ./sdk/ -run TestSDKFullPullAndGet
# 找到缓存文件,截成一半
truncate -s 50 /tmp/paladin-cache/paladin_public_prod.json
# 再跑 checksum 测试
go test ./sdk/ -run TestSDKCacheChecksumValidation -v
```

你会看到测试 PASS——因为截断后的 JSON 解不出来,或即使解出来也对不上 checksum,SDK 拒绝加载而不是带着半坏配置启动。

**Benchmark.** SDK 的长轮询请求周期 = PollTimeout 或变更到达时。一台空跑 SDK 的 CPU 基本为零,不像轮询 SDK 那种每秒一次 GET 的烧 CPU 模式。内存占用与配置数线性相关(每个 key 一份 []byte,加上一份 SHA-256 校验的字符串)。

## 6. Knowledge

**一句话本质.** SDK 的三段式生命周期把"永远可启动 + 增量同步 + 优雅关机"三件事串成一个可观测、可降级、可退避的状态机;SHA-256 校验把"缓存损坏"独立成一个明确错误分支。

**决策树.**

```
SDK 启动时服务端连不上?
├─ 本地有 cache + checksum 通过 → 用 cache 启动(降级)
├─ cache 不存在 → 空配置启动(调用方需容忍 nil 值)
└─ cache 存在但 checksum 不对 → 拒绝加载(避免带坏配置跑)

运行时服务端失联?
├─ 短暂失联(<1 轮 poll) → 下一轮自动恢复
└─ 长时间失联 → 指数退避 + 不 panic + metrics 报警

Shutdown 时的约束:
  Close() 必须等 goroutine 真正退出,否则 ctx 泄露
```

**知识图谱.**

```
Startup
  │
  ├─ fullPull ──成功──→ 内存 + cache(带 SHA-256)
  │        └──失败──→ loadFromCache
  │                      ├─通过→ 内存(降级态)
  │                      └─拒绝→ 空配置
  ↓
Runtime (goroutine)
  │
  └─ loop {
        select ctx.Done() → 退出
        long-poll Watch
          ├─ events → applyEvents + saveToCache + OnChange
          └─ error → sleep(backoff) continue
     }
  ↓
Shutdown
  cancel() → goroutine 在下一轮 select 检查时退出
  wg.Wait() → New 的调用方确信后台彻底停了
```

## 7. Transfer

**相似场景.** Nacos client、Apollo client、etcd client v3、Consul agent 全部走同一套三阶段生命周期,区别只在协议(长轮询 vs gRPC 流)和缓存格式(JSON vs protobuf)。理解 PaladinCore 的模式后读它们的源码基本不会迷路。

**跨域借鉴.** 前端 Service Worker 的 `install / activate / fetch` 三段式、浏览器 IndexedDB 的离线优先策略、移动端 App 的"先本地、后网络、再合并"的启动模式,本质都是同一个"永远可启动 + 增量同步 + 本地兜底"的 pattern。

**未来演进.** 当 SDK 需要跨进程共享配置(比如一台机器上 50 个同语言微服务),缓存可以升级为**共享内存 mmap**,任何一个 SDK 实例拉到最新配置后其他实例零成本同步。再进一步,可以把 SDK 拆成"agent(本机进程)+ thin client(业务进程 link 一个 so/.a)"——sidecar 模式的配置中心版本。

## 8. 回味

记忆锚点:**口诀"启动永不死,运行有退避,关机要等完;缓存带校验,半坏不启用"**;**一张"fullPull 失败 → cache 兜底 / watchLoop 出错退避 / Close 用 cancel+wg"的生命周期图**;**决策树:用 cache 还是空启动,看 checksum**。

**下一步.** 所有组件已经各自跑通,最后需要回答一个最朴素的问题:"怎么把它们凑一起,在我本地起一个最小可用的三节点集群"。下一篇把 Dockerfile、docker-compose 和本地 `cluster-local.sh` 脚本作为一个部署学习材料,完整呈现"从一张源码到一个运行中集群"的最后一步。

---

> **上一篇** ← [05. ForwardRPC 与 ReadIndex](./05-forward-consistent-read.md)
> **下一篇** → [07. 从脚本到集群:本地三节点拓扑的最小可复现环境](./07-cluster-deploy.md)
