# Day 6 — SDK 客户端测试

## 0. 本次修复的两处 Bug

| 位置 | 症状 | 根因 | 修复 |
|------|------|------|------|
| `server/server.go:41` | SDK 全量拉取 → `404 page not found` | 路由模式被改成了 `/api/v1/config`(无尾斜杠),`net/http.ServeMux` 把它当**精确匹配**,`/api/v1/config/public/prod` 不命中 | 改回 `/api/v1/config/`(子树匹配) |
| `sdk/client.go:49` | 全量拉取 HTTP 200 但 `Configs` 始终为空 | JSON tag 拼成了 `"config"`,而服务端发的是 `"configs"` | tag 改为 `"configs"` |

顺带把 `New` 拆成 `New` + `newBase`,避免 `RaftServer` 复用 `New` 时再次注册 `/api/v1/config/` 触发 ServeMux 的重复 pattern panic(Go 1.22+ 行为)。

---

## 1. 自动化测试(单元+集成)

```bash
# SDK 测试
go test -v -timeout 60s paladin-core/sdk

# 全量
go test -timeout 60s ./...
```

预期 `paladin-core/sdk` 五个测试全部 PASS:

| Test | 验证目标 |
|------|----------|
| `TestSDKFullPullAndGet` | 启动时 full pull,本地 `Get` 命中 |
| `TestSDKWatchUpdates` | `OnChange` 回调在服务端 PUT 后被触发 |
| `TestSDKCacheFallback` | 服务端宕机,客户端从本地缓存恢复 |
| `TestSDKCacheChecksumValidation` | 缓存被篡改 → SHA-256 校验失败 → 拒绝加载 |
| `TestSDKServerDown` | 服务端不可达且无缓存 → 不 panic,空配置 |

> ⚠️ 已知失败:`paladin-core/raft.TestRaftDelete` 是预存 bug(使用了未实现的 `Op{Type:"get"}`),与本次工作无关。

---

## 2. 端到端手动测试(SDK ↔ 真集群)

### 2.1 启动 3 节点 Raft 集群

```bash
# 清理旧数据(若有)
rm -rf data-1 data-2 data-3

# Terminal 1 — 引导节点(leader)
go run ./cmd/paladin-core cluster --id 1 --http :8080 --bootstrap
# 等待出现:
#   "entering leader state"
#   "Self-registered 1 -> 127.0.0.1:8080"

# Terminal 2 — follower
go run ./cmd/paladin-core cluster \
  --id 2 --raft 127.0.0.1:9002 --http :8081 --join localhost:8080

# Terminal 3 — follower
go run ./cmd/paladin-core cluster \
  --id 3 --raft 127.0.0.1:9003 --http :8082 --join localhost:8080
```

验证集群拓扑:
```bash
curl -s http://localhost:8080/admin/stats | grep -E 'num_peers|state|latest_configuration'
# num_peers 应为 2,latest_configuration 含 3 个 server
```

### 2.2 写入若干配置

```bash
curl -X PUT http://localhost:8080/api/v1/config/public/prod/db_host -d '10.0.0.1'
curl -X PUT http://localhost:8080/api/v1/config/public/prod/db_port -d '3306'
curl -X PUT http://localhost:8080/api/v1/config/public/prod/log_level -d 'info'
```

### 2.3 写一个最小 SDK 消费者

新建 `cmd/sdk-demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"paladin-core/sdk"
	"syscall"
	"time"
)

func main() {
	c, err := sdk.New(sdk.Config{
		Addrs:     []string{"127.0.0.1:8080", "127.0.0.1:8081", "127.0.0.1:8082"},
		Tenant:    "public",
		Namespace: "prod",
		CacheDir:  "/tmp/paladin-cache",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	c.OnChange("", func(key string, oldV, newV []byte) {
		fmt.Printf("[CHANGE] %s: %q -> %q\n", key, oldV, newV)
	})

	for k, v := range c.GetAll() {
		fmt.Printf("[INIT]   %s = %s\n", k, v)
	}

	// 周期性打印,验证缓存生效
	go func() {
		t := time.NewTicker(5 * time.Second)
		for range t.C {
			if v, ok := c.Get("public/prod/db_host"); ok {
				fmt.Printf("[POLL]   db_host = %s\n", v)
			}
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
}
```

运行:
```bash
go run ./cmd/sdk-demo
# 应输出 [INIT] 列出三条配置
```

### 2.4 在线变更 → SDK 实时收到

另开终端:
```bash
curl -X PUT http://localhost:8080/api/v1/config/public/prod/db_host -d '10.0.0.99'
# SDK 终端 1~2 秒内应打印:
#   [CHANGE] public/prod/db_host: "10.0.0.1" -> "10.0.0.99"
```

### 2.5 通过 follower 写入(forwardToLeader 验证)

```bash
curl -X PUT http://localhost:8081/api/v1/config/public/prod/log_level -d 'debug'
# 期望:200 OK + revision 递增
# 失败示例:若仍报 "unknown rpc type 80",说明 LeaderHTTPAddr 没拿到地址
#         (检查 /api/v1/config/__paladin/peers/1 是否已被复制到 follower)
```

### 2.6 Leader 故障切换 + SDK 容灾

```bash
# 在 Terminal 1 按 Ctrl+C 杀掉 leader
# 等 1~3 秒,新 leader 选出
curl -s http://localhost:8081/admin/stats | grep state   # 期望 Leader 之一
# SDK 不应中断,继续可读;通过新 leader 写入 → 仍可推送变更
```

### 2.7 集群整体宕机 → 缓存兜底

```bash
# 停掉 Terminal 1/2/3 全部节点
# SDK 终端会持续打印 watch error retry 日志
# 但 [POLL] 仍能读到最近一次缓存的 db_host(/tmp/paladin-cache/paladin_public_prod.json)
cat /tmp/paladin-cache/paladin_public_prod.json
```

---

## 3. 排错速查

| 现象 | 可能原因 |
|------|----------|
| `404 page not found` 返回给 SDK | `server.go` 的 `/api/v1/config/` 路由丢了尾斜杠 |
| SDK `GetAll()` 始终为空但 HTTP 200 | `configResponse` 的 JSON tag 不是 `"configs"` |
| follower 上 PUT → `unknown rpc type 80` | `forwardToLeader` 用了 `LeaderAddr()`(Raft TCP 端口),应用 `LeaderHTTPAddr()` |
| follower 上 PUT → `leader http addr unknown` | `__paladin/peers/{leaderID}` 还没被复制过来,等 1~2s 再试;或 bootstrap 节点没自注册 |
| `panic: pattern "/api/v1/config/" conflicts` | `NewRaftServer` 调用了 `New(...)` 而非 `newBase(...)` |
| watch 立即返回空且不阻塞 | 客户端传的 `revision` 已经包含了所有事件,等下一次写入即可 |
