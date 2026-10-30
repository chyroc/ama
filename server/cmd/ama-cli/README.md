# ama-cli —— 装进容器给 agent 用的 AMA CLI

容器里的 agent 靠它操作 AMA 自己的数据（空间 / Agent / 技能 / 运行历史 / 问题）。
从源码编（见下面「安装与构建」）；Docker 会话镜像 `sandbox-image/Dockerfile` 里自带一份。

```
容器里的 agent  --bash 工具--> ama 命令 --HTTPS--> amad 的 HTTP 接口
                                 ↑
                        凭据来自环境变量（平台注入）
```

**分工要分清**：这个 CLI 是**能力**，而 `core/skillbuiltin.go` 里那条 `ama` 技能是
**知识**（告诉模型有这个能力、怎么用、什么不能做）。**两者必须同时在**才有意义——
只有 CLI 没技能，模型压根不知道它存在，且不报任何错（`shared/infra/ma/agents.go:52`）。

## 三个环境变量

| 变量 | 必需 | 说明 |
| --- | --- | --- |
| `AMA_TOKEN` | ✅ | 凭据，放进 `Authorization: Bearer` |
| `AMA_BASE_URL` | | 服务地址，留空默认 `http://localhost:9999`（容器里由平台注入 `server.publicURL`） |
| `AMA_WORKSPACE_ID` | | 当前空间的 **id**（`ws_xxx`），多数命令的默认作用域 |
| `AMA_SESSION_ID` | | 当前会话 id（= 方舟 session id）；拼 `/api/runs/{id}/ark/...` 那组方舟透传接口（按会话列/挂文件、线程事件）时用它 |

⚠️ **`AMA_WORKSPACE_ID` 要传 id 不是 key。** CLI 直接拿它拼
`/api/workspaces/{id}/...`，传 key（URL 里那个短名）会得到 404，
**而那个 404 不会告诉你是标识搞混了**。

⚠️ **变量名必须逐字一致。** 名字对不上的后果是**零报错**：CLI 认为「平台没配置注入」，
打印一句劝退文案让 agent 去找用户，而注入其实成功了——日志里看到注入完成、
容器里却说没有凭据。常量在 `main.go` 顶部（`envToken` / `envBaseURL` / `envWorkspaceID`）。

## 🔒 凭据只从环境变量读，没有 `--token` 参数

这是硬约束，不是风格。容器里的命令行会**原样进 `tool_use` 事件 → 落 RunEvent
（永久记录）→ 进模型上下文 → SSE 推到浏览器**。一旦允许 `--token`，agent 某次
「顺手带上」就让那把凭据永久躺在数据库和用户浏览器里。

env 不进 argv：`ps` 看不到、shell history 记不下，且 `shared/infra/shelltools/scrub.go`
会把它从命令输出里抹掉。

**`ama api` 逃生口正是为此存在**：让 agent 调没封装的接口时不必手写
`curl -H "Authorization: Bearer $AMA_TOKEN"`。它不扩大权限面——能调什么由 amad 侧
决定，与走不走这个口子无关。

## 🔒 安全边界在 amad 侧，不在这里

容器跑的是 agent 生成的命令，它有 bash，**一句 `env` 就能拿到 `AMA_TOKEN`**。
所以「在 CLI 里禁掉危险命令」是**安全剧场**——拿到 token 自己 curl 就绕过了。

本程序只做一件与安全有关的事：`ama api` 的写操作要显式 `--yes`。它拦的是
**模型一次幻觉**这种无心之失，**不是恶意 agent**。⚠️ 两者不能混——以为客户端拦截
提供了实质保护，就会在服务端少加那道限制。

具名子命令（`issue create` 那类）不要这个闸门：它们的语义是明确的，而 `api`
能打任何路径，风险面完全不同。

## 报错要按「该不该重试」分类

**最贵的失败模式是 agent 拿到 403 后反复换参数重试**——那永远不会成功，只烧掉几轮
工具调用。所以 `describeHTTPError` 按 `code` 翻译成动作：

| code | 给 agent 的指引 |
| --- | --- |
| `access_denied` | **不要重试、不要换参数**，把原因告诉用户 |
| `not_found` | 资源真的不存在（不是没权限），用 list 确认 id |
| `unauthenticated` | **不要尝试登录**，容器里换不了这把凭据 |
| `invalid_argument` | 看 message 说了哪个字段 |
| 网络错误 | 这个**可以**重试 |

⚠️ **判据是 `code` 不是 HTTP 状态码**（spec 里明写过）：路由写错时标准库返回的 404
是 text/plain，按状态码判会把「路由挂错了」说成「资源不存在」。

## 联调时逮到的两个坑（都是「报错不指向真实原因」）

**一、`PATCH /api/issues/{id}` 对 title/body 是整体替换，不是局部更新。**
`core/issue.go` 的 `UpdateIssue` 无条件执行 `next.Title = draft.Title`，于是
「只改状态」那种请求（不带 title/body）会把两者一起清空，接着触发
「请至少填写标题或正文」被 400 拒掉——**报错完全不指向「你漏传了 title」**。
所以 `issue update` 先 GET 读回来、把现值填进 patch 再让显式参数覆盖。
只有 `status` / `priority` 是「不传即不改」。

**二、`ama api DELETE /path --yes` 曾报「不是合法 JSON」。** 按位置取第三个参数会把
`--yes` 当请求体。现在先按 `--` 前缀过滤再取位置参数。顺带给空响应体（204）打一句
「成功」——空输出会让 agent 怀疑命令没执行。

两个都只有真跑才暴露，单测照不出来。

## 为什么不从 OpenAPI 生成 client

那份 spec 从 `authz/matrix.go` 生成、有对账测试盯着，所以生成 client 确实永不漂移。
但覆盖 106 个操作的生成物有两个问题：产物入库后 review 全是噪音；更要紧的是
**它会诱导 agent 去调那些注定被拒的接口**。

所以只手写十来个高频子命令，**契约发现交给运行时**（`ama api-doc` 转发那份 spec，
它免鉴权、容器里一句命令就能读全量）。

## 安装与构建

开源版没有发包，从源码装：

```bash
git clone https://github.com/chyroc/ama && cd ama/server
go build -o "$(go env GOPATH)/bin/ama" ./cmd/ama-cli
```

会话容器里那份由 `sandbox-image/Dockerfile` 在构建镜像时现编，不用另装。
要交叉编译四个平台（linux/darwin × amd64/arm64）用 `./cli/build.sh`，产物落 `cli/bin/`、不入库。

⚠️ 二进制体积一律加 `-ldflags="-s -w"`（去符号表与 DWARF，省 30%），`build.sh` 与
`sandbox-image/Dockerfile` 都已经做了。`build.sh` 的版本号由 `-X main.version` 注入，
取自 `cli/package.json`——否则排查时「容器里装的是哪一版」只能靠猜。

## 怎么验证它真的可用

```bash
docker run --rm ama-sandbox:latest ama --version   # 镜像里有没有这个命令
```

再在一个勾了「AMA」授权的 agent 的对话里让它跑 `ama whoami`。三种失败的修法完全不同：

| 现象 | 真实原因 | 怎么修 |
| --- | --- | --- |
| 没有 `ama` 命令 | `docker.image` 换成了自己的镜像，里面没装 | 照 `sandbox-image/Dockerfile` 第一段编进去 |
| `没有凭据：环境变量 AMA_TOKEN 是空的` | agent 没勾 `ama` 授权，或 amad 没配 `auth.managedKeySecret`（启动日志会说） | 勾上 / 配上 |
| 能跑但连不上 | `AMA_BASE_URL`（= `server.publicURL`）在容器里不可达 | 见 [docs/guides/docker-executor.md](../../../docs/guides/docker-executor.md) 的「容器里的 `ama` 命令要能打回 amad」 |

## `ama skill upload`：上传技能包建技能（v0.4.0 新增）

技能可以**建**，也可以**上传**——这是两种创建方式。这个子命令走上传那条：把一个本地文件
（裸 `SKILL.md`，或内含 `SKILL.md` 的 zip 包）交给服务端解析并建技能。

```bash
ama skill upload ./my-skill/SKILL.md        # 裸 markdown（带 frontmatter）
ama skill upload ./my-skill.zip             # zip 包；顶层目录服务端会剥掉
ama skill upload ./my-skill.zip --dry-run   # 只解析、不落库，先看名字与文件清单对不对
```

要点：

- **服务端按文件名扩展名判形状**：`.zip` 走包解析，其余一律按 `SKILL.md` 解析。
  所以文件名必须原样带上（CLI 传的是 `filepath.Base`），改了名就换了解析方式。
- 上限 100 MiB（与 `skillsvc.MaxUploadBytes` 对齐）。CLI 在发之前先 `Stat` 拦一道，
  免得把上百 MiB 传出去再被服务端拒。
- `--dry-run` 的判定不能靠 `parseFlags`：它会把「--k 后面第一个非 -- 开头的 token」
  当成开关的值，于是 `upload --dry-run foo.zip` 里路径会被吃掉。布尔开关走 `hasFlag`，
  单测盯了两个方向——裸形式写在路径前面（路径不能被吃掉），以及 `--dry-run=true`
  这种带值形式（不能被漏认成「没给」：那会**静默地把技能真建出来**，本该只预览）。
- **这条不能走 `ama api`**：逃生口只发 JSON body，打不到收 `multipart/form-data` 的接口。

## `ama files`：个人文件（v0.3.0 新增）

把本机文件存到 AMA 的对象存储，按 AMA_TOKEN 的属主隔离，与空间无关。

字节存在服务端配置的 S3 兼容对象存储里（compose 默认是 RustFS）。服务端没配对象存储时
（只可能是 `server.store=mem`）`/api/files` 回 503。

```bash
ama files upload ./result.zip          # ≤8 MiB 直传；更大自动按 8 MiB 分片（上限 2 GiB）
ama files list                          # 列我已传完的文件
ama files get <uf_id>                   # 详情（含 1 小时有效的下载 URL，签给浏览器的）
ama files url <uf_id>                   # 只打印那条 URL——给浏览器 / 能直连对象存储的地方用
ama files download <uf_id> [本地路径]    # 经 AMA 中转下载（容器里用这个；默认用服务端记的文件名）
ama files delete <uf_id> --yes          # 删除（写操作要显式确认）
ama files abort <uf_id>                 # 取消一次没传完的分片上传
```

要点：

- 上传分片**每片失败自动重试 3 次**（同片号重传是幂等覆盖），全失败才报错并尽力
  abort，不留半成品；`--type` 可指定 MIME，`--name` 可覆盖展示文件名。
- `downloadUrl` 是对象存储的预签名链接，Host 签的是 `s3.publicEndpoint`（给浏览器的地址）。
  **会话容器里通常打不通**（compose 默认是宿主机的 `localhost:19000`，在容器里指向容器自己），
  所以容器里请用 `ama files download`：它打 `GET /api/files/{id}/content`，由 amad 流式转发，
  只要连得上 AMA 就能下。
- 打 `downloadUrl` 时**别带 AMA token**（凭据已在 URL 签名里），避免把凭据发给存储的域名。

## `ama daemon`：把本机变成宿主机执行端（v0.2.0 新增）

除了容器里给 agent 用的一组平台命令，这个二进制还能以 **daemon** 身份常驻在
**用户自己的机器**（Mac / Linux）上，让选定「宿主机执行端」的 agent 在这里跑
bash/read/write/glob/grep，工作区是持久目录 `~/.ama/runs/<sessionID>/`。

```bash
# 一条命令完成接入：首次运行先注册（需要用户 AMA_TOKEN；在 AMA 全局设置→API Key
# 创建；CLI 从源码装，见上面「安装与构建」），
# 再装成系统服务并启动。AMA_TOKEN 只在首次注册时用一次，之后服务只靠本地凭据。
AMA_TOKEN=amak_xxxx ama daemon install

# 前台调试 / 状态 / 日志 / 重启（升级 CLI 后执行 restart）
ama daemon run | status | logs | restart
ama daemon stop | uninstall
```

服务模式业务日志写 `/tmp/ama-daemon.log`，daemon 自己按 10MiB 切分、只保留
一份 `.1` 备份（约 20MiB 封顶，不需要系统 logrotate）；未捕获输出在 Linux 上
另进 journalctl、macOS 上在 `/tmp/ama-daemon.out.log`。

只想前台跑、不装系统服务时，才需要拆开两步：`AMA_TOKEN=... ama daemon register`
再 `ama daemon run`。已注册过的机器重装/升级服务直接 `ama daemon install`，
不再需要 AMA_TOKEN。

六工具（bash/read/write/edit/glob/grep）由 daemon 在本机用 Go 原生执行，
bash 走系统自带的 `/bin/bash`——**Mac 开箱即用，不需要安装 brew 依赖**。

daemon 注册后把服务端签发的 **host_token** 存进 `~/.ama/host.json`（0600），
之后只发出站连接、**机器不需要任何入站端口**：默认优先连 WebSocket
（地址按 AMA_BASE_URL 推导：同一个 host，http→ws / https→wss，路径
`/api/host-daemon/ws`），20s hello 帧续租、服务端有任务时主动推送；握手失败或连续短命断连时
自动降级回 20s HTTPS 心跳长轮询 + POST 批量回结果，并在后台每 5 分钟重探 WS
（老服务端回 404 时拉长到 30 分钟）。排障逃生：`AMA_DAEMON_TRANSPORT=http`
强制只走长轮询；`AMA_WS_BASE_URL` 可显式指定 WS 入口。服务端协议见
`shared/hostproto/messages.go` 与 `shared/hostproto/ws.go`，切换状态机见
`daemon_supervisor.go`，队列与掉线收敛见
`repository/schema/host_dispatches.sql` 与 `core/hostwatcher.go`。
在全局设置页「宿主机」可以看到机器在线状态与 GNU 工具探测结果、吊销机器；
空间设置「宿主机」把机器绑进空间后，Agent 编辑页才能选它做执行端。
