# 从脚本到集群:本地三节点拓扑的最小可复现环境

> Day 7 · PaladinCore 源码蒸馏系列 07
>
> 让"复现一个生产级的分布式 bug"从"申请三台 EC2、配 VPC、SCP 二进制"变成"一条 shell 命令"。

---

## 1. 引子:本地没有 Docker 的周末

你在周末想复现一个"Leader 切换时 Follower 转发 502"的 bug。按标准姿势,应该起一个三节点 Docker Compose,但手头这台机器没装 Docker,云端环境配置又太重。真正需要的其实只是:**三个进程,三组端口,一个 bootstrap**。这一篇把 PaladinCore 的三种部署形态——Docker 容器化、`docker compose` 多节点、本地 shell 脚本——放在同一个坐标系下讲清楚,并解释为什么一个看似"开发辅助"的 `cluster-local.sh` 脚本,实际是整个系统**可测试性**的底座。

## 2. 四层问题

**是什么.** PaladinCore 提供两条等价的多节点部署路径:`docker-compose.yml`(生产样板,三个容器,健康检查,卷持久化)和 `scripts/cluster-local.sh`(本地等价物,三个进程,相同的 HTTP/Raft 端口布局,相同的 bootstrap/join 流程)。两者共享同一个 `cluster` 子命令入口 `cmd/paladin-core/main.go`,只是参数来源不同。

**怎么做.** `cluster` 子命令做五件事:初始化 Raft 节点 → HTTP listen → 若 `--bootstrap` 则自举为单节点集群 → 若 `--join` 则向 Leader POST `/admin/join` 加入集群 → bootstrap 节点额外需要把自己的 HTTP advertise 地址通过 Raft 注册到 peer 表,方便后来的 Follower 转发时能定位到它。本地脚本在每次 `start_node` 里把 `--id`、`--raft 127.0.0.1:900X`、`--http :808X`、`--data data-nodeX` 和 `--bootstrap`/`--join` 五件事拼好。

**为什么.** 为什么还要本地脚本?Docker 其实并不复杂。原因有三:一、学习闭环下,启动容器需要重新拉镜像、卷映射、网络配置,调试"代码修改后重启验证"不如本地 `go run` 直接;二、CI / 开发环境常因企业策略没法跑 Docker,本地脚本是**降级路径**;三、三节点本地进程是最简单的"**可复现** bug 报告"载体——给同事发一条 `./scripts/cluster-local.sh --fresh` 比"我本地 Docker 见 Docker 没事"靠谱十倍。

**放弃了什么.** 本地脚本不模拟网络分区、不能模拟磁盘故障、端口是硬编码(靠 `HTTP_BASE`/`RAFT_BASE` env var 换)、没有滚动升级的概念——它是"**最小可运行**",不是"**尽可能真实**"。真要做 chaos engineering 还得靠 Docker + `iptables` 或 K8S + `chaosmesh`,本地脚本只覆盖"拓扑构造 + 基本功能验证"。

## 3. 类比

**生活类比.** 三种部署就像做饭:`docker-compose up` 是"叫外卖"(镜像即成品,容器编排即上菜流程)、`cluster-local.sh` 是"在家开火"(食材得自己备 / 环境变量自己设,但灵活)、直接跑 `go run ./cmd/paladin-core serve` 是"吃泡面"(最快最简,但只解决一顿)。不同场景选不同姿势,不必强求 Docker 至上。

**专业类比.** 本地脚本 vs Compose 就像 `make test` vs `CI pipeline`——前者追求 fast feedback,后者追求"和生产接近";两者共享同一套可执行产物,只在编排层不同。这正是为什么 `Dockerfile` 用多阶段构建:`builder` 阶段 `go build`,`runner` 阶段只带二进制,**本地脚本也能用同样的 `paladin-core` 二进制**,不需要任何 Docker 依赖。

**精妙类比.** `--bootstrap` 和 `--join` 的关系就像**第一个用户 vs 后续用户**:第一个人创建了这个群(bootstrap 自成单节点集群),后续用户通过"邀请链接"(Leader 的 `/admin/join` 端点)加入。Raft 不允许多个节点同时 bootstrap——群只能一个人创建,否则就是"两个同名群"撕裂,配置冲突。脚本里我们刻意让 `node1` 先 bootstrap 并**等它成为 Leader**,才让 `node2`/`node3` 执行 join,杜绝竞态。

**反类比.** 这个本地脚本**不是** `docker run --network host`。容器化的"host 网络"是"在容器里共享主机网络栈",而本地脚本**就是在主机上跑进程**——没有容器隔离,没有 PID 命名空间,没有 cgroup 限制。如果你带着 Docker 思维去问"脚本的 CPU 怎么限制",答案是 `ulimit` 或 systemd——不是容器那一套。

## 4. 揭秘

**入口:`cluster` 子命令五件事.** `cmd/paladin-core/main.go` 的 `runCluster` 函数是整个集群启动逻辑的唯一入口。它按顺序做:解析 flags → 创建 `raft.NewNode` → 套 `RaftServer` HTTP 层 → 若 `--join` 则 POST `/admin/join?id=X&addr=Y&http=Z` 到 Leader → 若 `--bootstrap` 则启动一个后台 goroutine 等自己当选 Leader 后调用 `RegisterPeerHTTP` 把自己的 advertise HTTP 写进 peer 表 → `http.ListenAndServe`。这个顺序的关键是"**bootstrap 节点必须自己注册 HTTP 地址**",否则后续 Follower 转发写请求时拿不到 Leader 的 HTTP advertise,就会拿到 503。

**Docker 与本地脚本的端口映射.**

```docker-compose.yml:5-22
node1:
  build: .
  command: >
    /app/paladin-core cluster
    --id node1
    --raft node1:9001       # 容器内网络,用 service name 解析
    --http :8080             # 容器内监听
    --data /data
    --bootstrap
  ports:
    - "8080:8080"            # 主机 8080 映射到容器 8080
  volumes:
    - node1-data:/data
  healthcheck:
    test: ["CMD", "wget", "-q", "-O-", "http://localhost:8080/healthz"]
    interval: 5s
```

Docker 里三个容器都 listen `:8080`,靠主机端口映射(8080/8081/8082)暴露。本地脚本没有容器隔离,三个进程必须 listen 不同的端口,所以 `scripts/cluster-local.sh` 里三个节点分别是 `:8080`/`:8081`/`:8082` 和 `:9001`/`:9002`/`:9003`。Raft 地址也相应从 `node1:9001`(docker DNS)变成 `127.0.0.1:9001`(本地回环)。

**本地脚本的一条关键保证:先等 Leader 再启 Follower.**

```scripts/cluster-local.sh:107-117
start_node node1 "127.0.0.1:$R1" ":$H1" data-node1 --bootstrap
wait_leader "http://127.0.0.1:$H1"          # 轮询 /admin/stats 直到 state=Leader
echo "==> node1 is Leader"

start_node node2 "127.0.0.1:$R2" ":$H2" data-node2 --join "127.0.0.1:$H1"
start_node node3 "127.0.0.1:$R3" ":$H3" data-node3 --join "127.0.0.1:$H1"
```

这条"等 Leader"比乍看之下重要得多。如果 `node2` 在 `node1` 还没自选为 Leader 时就调 `/admin/join`,请求会被路由到一个还没 commit 过任何 log 的初始 follower(其实 bootstrap 在它自己的 goroutine 里异步完成),返回 503——这是最常见的"脚本起不起来"的坑。用轮询等 Leader 出现是花 500 毫秒避免 3 秒诊断时间的廉价投资。

**端口冲突的预检.** 脚本在 `check_ports` 函数里逐一 `lsof -iTCP:$p -sTCP:LISTEN` 检查六个端口,任一占用立刻报错并给出两条自救方案(kill 占用进程 / 换端口)。这是从上一轮调试里学到的——用户笔记本上 `api` 进程(非 paladin)已经占了 8080,不做预检 node1 会在 `http.ListenAndServe` 那里挂掉,而 bootstrap 的 raft 层 log 会让人误以为是 Raft 问题。用 30 行 shell 把这类假阳性排除掉。

## 5. Experience

**MRE(完整本地集群 + 全链路).**

```bash
./scripts/cluster-local.sh --fresh

# 写入 Leader
curl -X PUT http://127.0.0.1:8080/api/v1/config/public/prod/db_host -d '10.0.0.1'
# 从 Follower 读(数据已复制)
curl http://127.0.0.1:8081/api/v1/config/public/prod/db_host
# 写到 Follower(触发 ForwardRPC)
curl -X PUT http://127.0.0.1:8082/api/v1/config/public/prod/via_follower -d 'ok'
# 观察三个节点状态一致
for p in 8080 8081 8082; do curl -s http://127.0.0.1:$p/admin/stats | python3 -c "import sys,json; s=json.load(sys.stdin); print(p:=$p, s['state'], 'rev='+s['store_revision'])"; done

./scripts/cluster-stop.sh --clean
```

**参数旋钮 1:端口换挡.** 默认 `HTTP_BASE=8080 RAFT_BASE=9001`,本地端口被占就换:

```bash
HTTP_BASE=18080 RAFT_BASE=19001 ./scripts/cluster-local.sh --fresh
# → node1 :18080/19001, node2 :18081/19002, node3 :18082/19003
```

这两行就是"预检 + 可配置"在真实场景的汇合点。

**参数旋钮 2:bootstrap 的唯一性.** 试试**两个节点同时 `--bootstrap`**:

```bash
# 手动起两个都 bootstrap 的节点,观察日志
./paladin-core cluster --id a --raft 127.0.0.1:9001 --http :8080 --data da --bootstrap &
./paladin-core cluster --id b --raft 127.0.0.1:9002 --http :8081 --data db --bootstrap &
```

两边都会认为自己是独立集群的 Leader,互不相识。这演示了 "bootstrap 只允许一个节点"的刚性约束。

**极端场景:杀 Leader 观察选举.**

```bash
kill $(sed -n 1p .cluster-pids)          # 杀 node1
sleep 3
curl -s http://127.0.0.1:8081/admin/stats | python3 -c "import sys,json;print(json.load(sys.stdin)['state'])"
# Leader     <- node2 或 node3 在 term+1 后当选
```

这一步把 Day 5 ForwardRPC 讨论的"选举窗口 503"变成可观察的实物——前 1-3 秒写请求拿 503,超过之后新 Leader 就绪。

**Benchmark 取向.** 本地脚本下 Raft apply 的延迟基本取决于磁盘 fsync,三节点 localhost 走 loopback,RTT 忽略不计。要做真正的集群延迟测试得把节点分到不同物理机。

## 6. Knowledge

**一句话本质.** Docker / Compose / 本地脚本是同一份 `cluster` 子命令的三层包装;拓扑正确性不在 Docker 而在**"先 bootstrap,再等 Leader,再 join"** 的启动顺序;peer HTTP 地址走 Raft 复制,这使得**拓扑元数据和业务数据共用一条强一致通道**。

**决策树.**

```
想起一个多节点集群?
├─ 生产 / 接近生产 → docker compose up -d
├─ 本地 / 没 Docker → ./scripts/cluster-local.sh
└─ 单机只跑逻辑测试 → go run ./cmd/paladin-core serve

端口冲突怎么办?
├─ Docker → 改 compose 的 ports 映射
└─ 本地 → HTTP_BASE=X RAFT_BASE=Y ./scripts/cluster-local.sh

要模拟"Leader 挂掉"?
├─ Docker → docker compose stop node1
└─ 本地 → kill $(sed -n 1p .cluster-pids)
```

**知识图谱.**

```
cmd/paladin-core/main.go → runCluster
        │
        ├─ raft.NewNode(config)
        ├─ RaftServer = server.NewRaftServer(node)
        ├─ if --join → POST /admin/join 到 leader
        ├─ if --bootstrap → 后台 goroutine 等 Leader → RegisterPeerHTTP(self)
        └─ http.ListenAndServe
                │
                ├─── 被 Dockerfile 打包成一个二进制
                │
                ├── docker-compose.yml 用 3 个 service 起 3 份
                │   (网络隔离,主机端口映射)
                │
                └── scripts/cluster-local.sh 用 3 个进程起 3 份
                    (localhost loopback,不同端口)
```

## 7. Transfer

**相似场景.** Consul、etcd、NATS 的本地多节点启动脚本、K3s 的 server/agent 拓扑、Redis Cluster 的 `create-cluster.sh`——全都是"单个二进制 + 若干 flag + 一条 bootstrap/join 协议"。理解 PaladinCore 的 `--bootstrap` / `--join` 就理解了这类分布式系统的启动协议。

**跨域借鉴.** K8S 的 `kubeadm init` + `kubeadm join` 是同一套模式在容器编排领域的标准化;CockroachDB 的 `cockroach start --join`;OpenSearch 的 `cluster.initial_master_nodes`——本质都是"**谁先立,谁后加**"的两段式启动。

**未来演进.** 当你的集群从 3 节点扩到几十、几百节点,手工 bootstrap + join 就变成运维噩梦。下一步一般是引入**服务发现**(Consul / etcd 本身就是服务发现,有点循环依赖,所以更常见是 DNS SRV 或 K8S StatefulSet)。PaladinCore 当前没走那么远,因为配置中心的集群规模通常 3-5 节点就够——**不为不需要的规模过度设计**也是工程美学的一部分。

## 8. 回味

全系列到这里收束。七天建起的东西——一份带 revision 的 KV、一套 HTTP 路径、一段长轮询、一个 Raft FSM、一层 ForwardRPC、一个可降级的 SDK、一键起的三节点——每一片都是"解决上一片遗留的一个具体缺陷"的自然延伸。真正的价值不是"你学会了配置中心",而是学会了**分布式系统的七种思维工具**:逻辑时钟、资源命名、推代拉、共识日志、透明路由、三阶段客户端、可复现拓扑。它们在 etcd、K8S、Kafka、TiKV、Consul、Nacos、Apollo 的各个角落反复出现——读 PaladinCore 就是预习这些系统的一张地图。

**下一步.** 按下面五件事继续推进你的理解最划算。第一,把整套源码过一遍测试 `go test ./... -race -count=1`,亲历系统的全部不变量。第二,打开 etcd 的 `mvcc` / `raft` 包,用 PaladinCore 的心智模型去对号入座,你会发现大部分代码你已经读得懂。第三,到 HashiCorp `raft` 库的 `raftboltdb` 源码里看看 Raft log 本身是怎么存的——"我们用 BoltDB 存业务数据,它也用 BoltDB 存 Raft log"这一对称性非常教学。第四,尝试把 PaladinCore 的 Watch 协议换成 gRPC 双向流,你会亲身感受到"协议选择"的约束面。第五,给 PaladinCore 加一个"JWT 鉴权 + 审计日志"的 PR,那是生产化的最后一公里。

---

> **上一篇** ← [06. SDK 三阶段生命周期](./06-sdk-fallback.md)
> **回到总览** ↺ [系列索引](./README.md)
