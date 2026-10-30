# dockertools：每个方舟会话一个 Docker 容器

方舟 Managed Agents 的 `self_hosted` 环境里，方舟只做推理，工具调用
（bash / read / write / edit / glob / grep）由 amad 里的 worker 领走执行。
本包就是默认的执行端：**每个方舟会话一个容器，六个工具全部经 `docker exec`
落进同一个容器**，共享同一个文件系统。

底下的 Docker Engine API 客户端在 `shared/infra/docker`（net/http 直连 unix socket，
没有引 `github.com/docker/docker`）。六个工具本身在 `shared/infra/shelltools`，
与执行端无关；本包只负责「命令落到哪个容器、容器什么时候建 / 暂停 / 删」。

另一个执行端是用户自带的机器（`service/host` + `shelltools.NewNativeSet`），
agent 选了宿主机时 `core/toolrouter.go` 按 run 上的 `host_id` 分流过去。

## 一个会话的东西

| 东西 | 名字 | 生命周期 |
| --- | --- | --- |
| 容器 | `<prefix>-sess-<会话 id>` | 第一次真调工具时建；空闲后 pause / stop；闲置超过 `containerTTL` 被删 |
| 工作区 volume | `<prefix>-ws-<会话 id>` | 挂在 `workdir`（默认 `/workspace`）；闲置超过 `workspaceTTL` 才删 |
| label | `<prefix>.managed=true`、`<prefix>.ark-session=<id>`、`<prefix>.kind=session/workspace` | 回收巡检只认带这些 label 的东西 |

名字是由会话 id **算出来的**（方舟的 `sesn-xxx` 原样可用，含别的字符时退成 sha256 前缀），
任何实例拿到会话 id 都能直接找到容器，不需要进程内的映射表。

**工作区单独放 volume** 是这个设计里最重要的取舍：容器是随时可以丢的东西（daemon 重启、
回收巡检、有人手动 `docker rm`），而 agent clone 的仓库、装的依赖不该跟着丢。
容器没了只是下一次调工具多花一两秒重建，文件都还在；只有 volume 也没了才是
「工作区已重置」。日志把这三种情况分开说：

- `容器已创建（首次调用工具时懒建）`
- `容器不存在，已重建——工作区 volume 还在，之前的文件都保留着`
- `容器与工作区 volume 都已不在（被回收或被手动删除），已新建——工作区已重置`（Warn）

## 状态机

每次工具调用先 `ensureRunning`，**按 daemon 当前的真实状态走**（inspect 现查，
只有一个 2 秒的「刚确认过在跑」短缓存）：

```
不存在           → 建 volume + 建容器 + 启动 → 记账 → 注技能
                   （409 名字已占用 = 别的实例刚建好 → 重新 inspect 复用它）
paused           → unpause
created / exited → start（后台进程不会回来，volume 在）
removing / dead  → 强删后重建
running          → 直接用
```

exec 时撞上 409（容器在「确认在跑」与「exec」之间被别的实例暂停了）或 404（被删了），
清掉短缓存重来一次，不把这种竞争报给 agent。

### 空闲

一条 work 收尾（SDK 调 `Set.Close`，即 `end_turn` 后空闲满 MaxIdle，默认 60 秒）时：

| `idleAction` | 做什么 | 恢复代价 |
| --- | --- | --- |
| `pause`（默认） | cgroup freezer 冻住全部进程，内存保留 | 毫秒级，进程接着跑 |
| `stop` | 停掉容器，内存释放 | 一两秒，进程全没了，volume 在 |
| `none` | 不动 | — |

三种情况跳过这一轮：这条 work 根本没碰容器（纯文本回复）、**网页终端还开着**
（查 PG 的 `terminal_active_until`，终端请求与空闲收尾几乎不在同一个进程）、
**容器里还有 `run_in_background` 起的后台任务在跑**（每次都 exec 进去查 pid 文件，
不靠进程内标志：后台任务可能是上一条 work、甚至别的实例起的）。

### 命令超时

Engine API 没有「杀掉某个 exec」的接口，ctx 取消只会让我们不再等，进程还在容器里跑。
所以 `ExecSpec.Timeout > 0` 时命令被包成 `timeout -k 5 <秒> <命令>`：到点 TERM，
再过 5 秒 KILL。退出码 124 / 137 会翻成一句明确的「已在容器里被终止」放进 `Result.Err`。
**这要求镜像里有 coreutils 的 `timeout`。**

## 多实例

线上 amad 是多实例的，每个实例可能连**同一个** daemon，也可能各连各的。

- **不在进程内记「哪个会话用哪个容器」**。会话句柄每条 work 新造一个，用完即弃；
  状态一律 inspect 现查。
- **抢建不会双建**：容器名是确定的，daemon 保证同名只能建一个。抢输的实例拿到 409，
  转去复用对方的。volume 同理（`VolumeCreate` 对已存在的同名 volume 直接返回它）。
- **落 PG 的三样**（`run_sandbox_sessions` 表，由 core 实现本包声明的接口）：
  - 容器名 + 所在 daemon 的 id（`RecordSession`，建出新容器时写）——页面请求落到任意实例，
    靠它区分「没建过（none）」与「建过但没了（gone）」，以及「容器在别的 daemon 上」；
  - 最后活动时刻（`TouchSession`，节流到 30 秒一次，时刻由数据库 `now()` 算）——回收巡检用；
  - 终端在用到什么时候（`terminal_active_until`）——空闲收尾用。
- **回收巡检先抢锁**：Redis 租约，键 `<prefix>:docker-reaper:<daemon id>`。按 daemon 分，
  连不同 daemon 的实例各扫各的；没配 Redis 时每个实例都扫（删除幂等，结果正确，只是重复）。

⚠️ **连不同 daemon 的实例之间不共享工作区。** 会话的 work 落到连着另一个 daemon 的实例上，
那边会建一个新容器、用它自己 daemon 上的 volume——对用户来说就是工作区被重置了。
页面上看容器时，库里记的 daemon 与本实例不一样会报「容器在另一个 Docker daemon 上」
（**不是**「已回收」）。要让工作区跨实例，所有实例必须连同一个 daemon（`docker.host`）。

## 回收巡检

`RunReaper` 每 10 分钟一轮：

- 容器闲置超过 `containerTTL`（默认 24h）→ 删容器，volume 保留；
- volume 闲置超过 `workspaceTTL`（默认 7 天）→ 先删同名容器，再删 volume；
- 「闲置」按活动账本算；账本里没有（孤儿）的退到容器 / volume 自己的创建时刻；
- **账本读不出来就整轮跳过**：退到创建时刻会把正在用的老会话误删；
- 只碰带 `<prefix>.managed=true` 的东西。⚠️ 同一个 daemon 上跑两套互不相干的 AMA 时
  **必须配不同的 `prefix`**，否则会删到对方的容器。

worker 退出时**不删容器**：容器属于会话不属于进程，滚动发布时别的实例还在用。

## 页面上的「看容器」

`Provider.Attach` 给 core 的 `DockerSandboxAttacher` 用：查状态（running / paused / stopped，
「容器被删了但 volume 还在」也算 stopped，可以恢复）、恢复、列目录（GNU `find -printf`）、
读文件（`head -c`）、webshell（每条命令一次 exec，cwd 存在容器里的小文件里，按 shellID 分）、
网页终端。

**网页终端**是 `docker exec -it bash -l`（TTY + hijack 出来的裸连接）。`Terminal` 对外长得像
一条收发 JSON 文本帧的 WebSocket，帧形状就是浏览器终端本来说的那套
（`input` / `resize` 进，`output` + `dataB64` 出），所以 core 里 SSE 与 WS 两条转发路径都不用改。
连接握在开终端的那个实例手里，跨实例的按键照旧经 pgmq 转发给它（见 `core/sandboxterm.go`）。

## 镜像

默认 `ama-sandbox:latest`，用仓库根的 `sandbox-image/Dockerfile` 构建（带 bash、coreutils、
GNU find、grep、git、python3、ripgrep、jq，以及从源码编的 `ama` CLI）。不在本地时建容器
那一刻会去拉一次；拉不到的报错会带上构建命令。自己换镜像满足四样即可：
**bash、coreutils（含 `timeout`）、GNU findutils、grep**。

容器的 entrypoint 由我们覆盖成一个收到 TERM 立刻退出的保活循环（不用 `sleep infinity`：
busybox 不认），并开 `Init`（tini 当 PID 1，回收后台任务的僵尸进程）和 `no-new-privileges`。

## 安全

⚠️ **amad 能访问 docker.sock 就等于拥有宿主机 root。** 会话容器本身拿不到 socket，
但它们与宿主机共享内核；容器隔离不是虚拟机隔离。要更强的隔离见
[docs/guides/docker-executor.md](../../../../docs/guides/docker-executor.md)。

## 测试

- `go test ./shared/infra/docker/`：httptest 模拟 Engine API，含多路复用解帧、hijack 写 stdin、
  拉镜像时藏在进度流里的错误。
- `go test ./shared/infra/dockertools/`：内存假 daemon 测状态机、抢建、空闲、回收、Attach。
- `integration_test.go` 打**真 daemon**（连不上就 `t.Skip`，跳过时是绿的）：建容器 → bash 写文件
  → read 读到 → 暂停 → 恢复后再读到 → 删容器后重建、文件还在 → 列目录 / webshell / 终端
  → 删除；以及「超时的命令真的被杀掉了」。镜像默认 `debian:bookworm-slim`，
  `AMA_DOCKER_TEST_IMAGE` 可换。
