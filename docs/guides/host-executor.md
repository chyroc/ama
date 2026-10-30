# 宿主机执行端（host executor）

让 agent 的工具（bash/read/write/edit/glob/grep）跑在**用户自己注册的机器**
（Mac/Linux）上，而不是默认的 Docker 会话容器（见 [docker-executor.md](docker-executor.md)）。
机器上跑一个常驻的 `ama daemon`，工作区 `~/.ama/runs/<方舟 sessionID>/` 是持久的。

## 三个实体与两级绑定

```
hosts（账号级）         ama daemon install 首次运行自动注册产生：owner_email、
                        host_uuid、token_hash、last_seen_at、native_tools
                        （也可先单独 ama daemon register，只注册不装服务）
workspace_hosts（空间级，可选） 空间设置绑定：只能绑本人拥有的主机
agents.executor_type/host_id/host_source   Agent 编辑页选择（两级来源）
runs.host_id/host_source                   建会话时快照
```

宿主机是**个人**资源（一台机器只属于注册者），但挂到 agent 上有两条路，与
「个人授权 / 服务账号授权」是同一组对立：

- **空间指派（host_source=workspace）**：属主先把机器绑进空间（workspace_hosts，
  仍然只能绑本人的机器）；之后**空间管理员**可以把空间里任意一台已绑机器（含别人
  绑进来的）指派给 agent，机器属主本人也能选自己绑进来的机器。空间解绑后，存量
  agent 下次建会话被挡住。
- **个人直连（host_source=personal）**：agent 创建者在编辑页直接选自己注册的
  机器，**不经过 workspace_hosts**。只有 `agent.CreatedBy` 本人能改这个选择
  （空间管理员也不行；存量没记 created_by 的 agent 不允许配个人直连，要管理员
  重建 agent）。空间解绑与它无关，唯一的失效事件是属主吊销机器。

`host_source` 是可空列：存量行与滚动发布期旧二进制写 NULL，读路径经
`apimodel.EffectiveHostSource` 一律按 workspace 解释（= v1 行为），不做数据回填。
写路径唯一闸门是 `service/agent/ensureExecutorAllowed`（形状对称于
ensureIntegrationsAllowed），判「变没变」而不是「传没传」——非创建者只改名字、
完整回传草稿不受影响。

agent 保存合法性：
- workspace：机器存在 + 已绑空间；操作人是空间管理员**或**该机器属主；
- personal：机器存在 + 属主是 actor，且 actor 是 agent 创建者本人；
- 切回云容器（清空）是收权，管理员或原机器属主可操作。

agent 选了「宿主机」后，创建 run 时把 host_id 与来源快照到 `runs`；建会话前按来源
复查（`core/chat.go`：workspace→仍绑定，personal→机器仍在）。工具分流
（`core/toolrouter.go`）只读 run.host_id、对来源无感知，agent 之后改执行端不回溯
历史 run。

## 通信模型：daemon 全出站，队列以 PG 为准

daemon 不监听任何端口（NAT 后的笔记本开不了入站）。四条路由，新 daemon
默认走 WebSocket，连不上自动降级回 HTTP 长轮询（两条路服务端永久并存）：

| 路由 | 鉴权 | 说明 |
|---|---|---|
| `POST /api/host-daemon/register` | 用户 Bearer（ActionNone） | 幂等注册，首次签发随机 host_token（库存 sha256）。始终走 HTTP |
| `GET /api/host-daemon/ws` | host_token（握手 Authorization 头，裸 mux 自鉴权） | WebSocket 优先路：20s hello 帧续租，NOTIFY 唤醒后服务端主动推 dispatches，results 帧必须等 results_ack |
| `POST /api/host-daemon/heartbeat` | host_token（裸 mux 自鉴权） | 20s 长轮询，返回待执行 calls（WS 降级路 + 老版本 daemon） |
| `POST /api/host-daemon/results` | host_token（裸 mux 自鉴权） | exec 结果 CAS 落库 / 终端帧中继（降级路 + WS 写失败的无状态兜底） |

为什么要降级而不是只用 WS：不少网关/负载均衡对主域名协商 HTTP/2，Upgrade 头被吞掉，
WS 握手必失败；专用的 HTTP/1.1 入口又常有几十秒的静默断连上限，用户网络/公司代理对
WS 的穿透也不可控。daemon 因此：WS 地址默认按 BaseURL 推导（host 不变、http→ws），
可用 `AMA_WS_BASE_URL`（或 host.json 的 `wsBaseUrl`）显式指到单独的 WS 域名；拨号失败（404/405/网关无规则）
直接降级并 30 分钟重探一次，连续 3 次短命断连降级 HTTP、之后每 5 分钟重探，活过 60s
的连接清零失败计数；双向 20s 保活（daemon hello + 服务端协议 ping）顶住网关空闲上限。
`AMA_DAEMON_TRANSPORT=http` 是只走 HTTP 的逃生开关。状态机见
`cmd/ama-cli/daemon_supervisor.go`。

host_token 心跳/结果/WS 不走 auth.Protect：它不是用户凭据，且 20s 一次的心跳不能
触发用户 API Key 的 TouchAPIKey 写放大。它和终端 WS 的一次性票同类，在
`apihandler/handler_hostdaemon.go` 的裸 mux 上自鉴权（不进 authz matrix），
安全由 `hostdaemon_test.go` / `hostdaemon_ws_test.go` 钉住。

WS 不引入任何只活在连接上的状态：域层会话（`service/host/stream.go`）复用
HTTP 长轮询同一组 touch/claim/wait 原语，正确性事实源仍是 PG。同一台主机在
本实例内的新连接踢旧连接（close 4002），跨实例靠 SKIP LOCKED 分流 + 租约收敛，
不引入跨实例锁。

下行队列是 `host_dispatches`：per-host 单调 seq（入队事务内
`pg_advisory_xact_lock(hashtextextended(...))` 串行分配），daemon 持久化
lastSeq。`claim` 原子认领 queued（游标之后）与 delivered 租约过期（exec 150s /
term 30s）两类行——后者是崩溃/丢结果后的重投，daemon 内存里的 exec 账本
（`cmd/ama-cli/daemon_ledger.go`，256 条/32MiB 有界）按 callID 去重：已跑完
的 call 重投只回缓存结果、绝不重跑有副作用的脚本；账本不持久化，daemon 重启
后重跑是既有兜底（服务端 CAS 只认第一次结果）。

唤醒**不靠轮询**：入队/完成在同一事务 `pg_notify('ama_host_wake', host_id)`，
cmd/amad 一条独占 pgx 连接 LISTEN（`cmd/amad/hostwake.go`），
`core/hostwake.go` 在本进程 fan-out 给心跳挂起与 exec 等待；NOTIFY 可能丢，
心跳 20s 必返回、exec 另有 1s ticker 兜底。

## 掉线与离线错误

90s 没心跳 → `core/hostwatcher.go`（Redis 选主 + PG CAS）把主机置 offline、
把未完成 exec 置 done + `host_offline` 结果并 NOTIFY。exec 外层等待封顶 130s
（六工具 120s 超时 + 15s 余量），**必须小于 workerfailover 的 180s**——那个
看门狗 180s 无工具结果会把整条 work 交回方舟重派。

## 工具实现：宿主机走 Go 原生协议，Docker 容器走 shell 脚本

两条执行端的工具实现**刻意不同**，但凭据注入/抹密与技能编排共用同一份：

- **Docker 容器**：`shared/infra/shelltools` 拼 GNU 方言 shell 脚本（stat -c /
  find -printf / base64 -w0 / setsid），经 `docker exec` 在容器里 `bash -lc` 执行。
  所以会话镜像必须带 GNU 用户态（`sandbox-image/Dockerfile` 用的是 debian）。
- **宿主机 daemon**：`shared/hosttools`（纯标准库）提供结构化原生实现。
  amad 下发的不是脚本而是 `kind='tool'` 的 `hosttools.ToolCall`（工具名 +
  原始 tool_use JSON；bash 另带合并好的 env），daemon 在本机：

  - read/write/edit 用 os 直接读写（os.Stat/ReadFile/MkdirAll/WriteFile）；
  - glob/grep 用 filepath.WalkDir + Go regexp(RE2)（顺带消除 GNU ERE 与 RE2
    的已知偏差）；
  - bash 由 Go fork **系统自带** `/bin/bash -lc`，Setpgid 进程组 + 超时
    SIGTERM/SIGKILL；后台任务用 `SysProcAttr{Setsid:true}` + /tmp/ama-bg
    文件布局（agent 的 tail/cat/kill 提示与旧版逐字一致）；
  - 技能注入是两个内部操作 `__ama_write_files`（JSON 传内容，彻底绕开
    MAX_ARG_STRLEN 128KiB）与 `__ama_fetch_archive`（net/http 下载 + 字节
    数校验 + archive/zip 解压 + Zip Slip 防御，支持内容指纹本地缓存）。

因此 **Mac 零 brew 依赖**：不需要 coreutils/findutils/util-linux，bash 用
macOS 自带的 /bin/bash（3.2，agent 命令都是普通 `-lc` 语法）。

共用件：凭据/env 合并、别名、出口抹密在 `shelltools.SessionInjector`
（容器的 Execer 装饰器与宿主机的 `shelltools.NewNativeSet` 都用它）；技能
注入编排（并发 4 / 令牌后计时 / warn-only 回落）在 hosttools 与 shelltools
各有一份同语义实现，因为线格式不同（JSON vs shell argv）。

## 能力位：native_tools 与旧 daemon 闸门

注册/心跳上报 `native_tools`（hosts.native_tools 列），新 daemon 固定 true。
不支持原生协议的旧版 daemon（字段缺省 false）上执行工具时，amad 在入队前
直接回「请升级 ama daemon」的结构化错误——它只认旧 `kind='exec'` 脚本
协议，而新代码不再下发那种调用。旧的 `gnu_tools_ok/toolcheck` 两列保留给
老版本心跳与历史数据，新代码不读不写（仓库惯例不做删列迁移）。

## 网页终端

浏览器 WS 复用容器终端同一条 URL 与一次性票；`core/sandboxtermws.go` 在核销
票后按 `run.host_id` 分流：容器版在会话容器里 `docker exec -it bash -l`，宿主机版
（`sandboxtermws_host.go`）把 open/input/resize/close 写成 term_* dispatch，
daemon 在本机用 creack/pty 拉起 login shell，PTY 输出随 results 上报，经
`DeliverHostTermFrame`（本机 terminalHub 直写 / pgmq topic `term:<id>` 中继）
回浏览器，输出与 webshell 过同一道凭据 scrub。

**票路由（`POST .../terminal/ticket`）是 ActionRead，真正的授权在
`core.IssueTerminalTicket` 里按 `run.host_source` 分三档 fail-closed**
（matrix 判不了「这台机器是不是你的」这类行级关系）：

| run 类型 | 谁能开终端 |
|---|---|
| 容器 | 空间 owner/admin |
| 空间指派宿主机（含存量空 source） | 空间 owner/admin（v1 语义） |
| 个人直连宿主机 | **只有机器属主本人**（按 run.HostID 反查 hosts.owner_email；他在空间里只是 member/viewer 也行） |

档位看的是 **run 快照**（建会话时定的），agent 后来改来源不影响历史 run 的终端。
WS 本身仍只认证、凭票核销，runID 配对校验不变；host 终端的输入走 WS 帧，
`POST .../terminal/input`（仍 ActionManage）对 host 会话显式拒绝，防止管理员
绕过属主档直接写按键。

## 个人直连的安全语义

- **对话不代表终端**：空间任何成员与该 agent 对话，工具都会在**创建者的机器**上
  执行（这是刻意的，与个人授权「agent 以创建者身份行动」同口径）；但网页终端
  只有机器属主本人能开。
- 个人机器被属主吊销（DELETE hosts）后：建会话被挡、终端被挡；agent 行保留
  host_id 软引用，编辑页显示「宿主机已被删除」，由创建者改回云容器或换机器。
- 空间解绑只删 workspace_hosts，不影响任何 personal 引用（刻意不加外键）。

## 明确的边界

- 空间绑定只能绑**本人属主**的机器（空间 owner/admin 也不能替别人绑）；
  但绑进空间后，管理员可以把它**指派**给任意 agent（选机器这一步放开了 owner
  限制，v1 是「属主本人才能选」）。
- daemon 自动安装系统依赖、跨主机 failover 没有——主机离线时工具明确报错。
- token 复制到两台机器跑不做 owner fencing（只文档声明）。
- **空间 owner/admin 都能对「空间指派」的主机开网页终端**：等于其他管理员可在
  主机属主的机器上执行命令；个人直连不受此限（只有属主）。指派机器给空间共用时
  请知悉。
- exec 行的 payload（注入凭据）与 result（可能含明文输出）保留 7 天由 hostwatcher
  自动清除；终端帧保留 10 分钟。
