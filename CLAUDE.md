# CLAUDE.md

本文件给在本仓库里工作的人和 coding agent（如 Claude Code）提供指引。
项目介绍见 [README.md](README.md)，部署与配置见 [docs/deployment.zh-CN.md](docs/deployment.zh-CN.md)，本地开发见 [docs/development.zh-CN.md](docs/development.zh-CN.md)。

## 项目意图

AMA 平台：后端 Go，前端 React（Vite），构建在**方舟 Managed Agents** 之上，
agent 的工具跑在**我们自己的执行环境**里（方舟 `self_hosted` 环境）：默认是每个会话一个
Docker 容器，也可以是用户用 `ama daemon` 注册的机器。

## 这是分布式服务，不是单机服务

**amad 按多实例设计，写代码时按「同一份代码同时有 N 个进程在跑」来想。**

本地 `./dev.sh` 起一个进程，很容易让人形成「单机服务」的直觉，于是顺手做出一批
多实例部署时必然出错的决定。这类故障有个共同点：
**单实例跑测试全绿、部署后偶发，且报错不指向真实原因**——表现是「有时候好有时候不好」，
最难查的一类。

三件事决定了它必须按分布式写：

1. **生产环境通常是多实例**，请求经负载均衡随机落到任一实例——两条相邻请求通常不在同一个进程里；
2. **滚动发布期间新旧实例同时在线**，所以哪怕实例数配成 1，也存在两个进程并存的窗口。
   「我只部一个实例」不能作为写进程内状态的理由；
3. **实例随时会被重建**（健康检查失败、发布、缩容），进程内的东西说没就没，且不通知任何人。

### 不能这么做 / 要这么做

| 场景 | 别做（单机思维） | 要做 |
| --- | --- | --- |
| 需要跨请求记住的东西 | `map` / `sync.Map` / 包级变量 | 落 PostgreSQL（`shared/infra/pg`） |
| 互斥、选主、"只能有一个" | `sync.Mutex` | Redis 租约（`shared/infra/redis`，飞书长连接就是这么做的） |
| 定时任务、后台巡检 | 每个实例各起一个 ticker | 先抢锁，抢不到就不跑 |
| 有远端副作用的创建 | 先查再建（TOCTOU） | 唯一约束 + `ON CONFLICT`，让数据库裁决 |
| 读改写 | `SELECT` 完在 Go 里改再 `UPDATE` | 单条 SQL，或事务 + 行锁，或乐观版本号 |
| 外部回调 / 消息去重 | 进程内 seen 集合 | 幂等键入库，唯一约束兜住重复投递 |
| 长连接 / SSE 订阅者 | 进程内订阅表就够了 | 订阅者在 A、事件在 B，要想清楚怎么落库与怎么送达 |
| 自增 ID、序号 | Go 里 `n++` | 数据库序列，或 UUID 之类无中心生成 |
| 临时文件、缓存目录 | 当成一块共享磁盘 | 每个实例的磁盘是各自的，重建即丢；要共享就外部存储 |

**进程内只允许放能随时丢、丢了也只是变慢的东西**（纯缓存、连接池、当次请求的中间态）。
凡是「丢了功能就不对」的，都必须在进程外。

写完对着问一句：**这段代码有两个进程同时在跑时，还对吗？**
如果答案依赖「只有一个实例」，那它就是个待爆的雷。

## 按场景读文档

**本文件只放每次都要知道的东西。** 具体到某类改动才需要的细节在 `docs/guides/` 与
各包 README 里，动手前按下表取用——那些文档里几乎每一条都是踩过的坑。

| 你要做的事 | 先读 |
| --- | --- |
| 改 `server/` 的 Go 代码 | [docs/guides/backend.md](docs/guides/backend.md) |
| 改 `web/src` 的前端代码 | [docs/guides/frontend.md](docs/guides/frontend.md) + `docs/DESIGN.md`（见下「碰 UI 之前先读约定」） |
| 用 HeroUI 组件 | [docs/guides/heroui.md](docs/guides/heroui.md) |
| 改主题 / 配色 / 深色 / 圆角 | [docs/guides/styling.md](docs/guides/styling.md) |
| 增删界面文案 | [docs/guides/i18n.md](docs/guides/i18n.md) |
| 换 logo / 改 favicon / 品牌位 | [scripts/brand-icons/README.md](scripts/brand-icons/README.md) |
| 加模型 / 改模型厂商标 | [scripts/model-icons/README.md](scripts/model-icons/README.md) + [server/shared/infra/ma/README.md](server/shared/infra/ma/README.md) |
| 碰 `server/shared/infra/` 的接入层 | [docs/guides/infra.md](docs/guides/infra.md) + 该包 README |
| 碰 Docker 执行端 / 会话容器 | [docs/guides/docker-executor.md](docs/guides/docker-executor.md) + [server/shared/infra/dockertools/README.md](server/shared/infra/dockertools/README.md)（**docker.sock 等于宿主机 root**） |
| 碰自带机器执行端（`ama daemon`） | [docs/guides/host-executor.md](docs/guides/host-executor.md) |
| 加 HTTP 路由、改权限/角色 | [server/authz/README.md](server/authz/README.md) |
| 加/改接口后同步 OpenAPI、写给 agent 看的说明 | [server/openapi/README.md](server/openapi/README.md) |
| 加/改配置项、动配置来源 | [server/shared/infra/config/README.md](server/shared/infra/config/README.md) |
| **加一张表 / 改 DDL / 找某张表的定义** | [server/repository/schema/README.md](server/repository/schema/README.md)（**字段展开成列，别塞 doc jsonb**） |
| **给 Store 加方法 / 加存储层类型 / 动 jsonb doc 的 json tag** | [server/model/persistmodel/README.md](server/model/persistmodel/README.md) |
| 碰 PostgreSQL 连接 / 连接池 / 超时 | [server/shared/infra/pg/README.md](server/shared/infra/pg/README.md) |
| 碰 Redis 租约 / 选主 | [server/shared/infra/redis/README.md](server/shared/infra/redis/README.md) |
| 碰对象存储 / 附件与文件字节 / 预签名链接 | [server/shared/infra/s3/README.md](server/shared/infra/s3/README.md)（**字节在对象存储、元信息在 PG；三个 endpoint 不能混用**） |
| 碰飞书机器人 | [server/feishu/README.md](server/feishu/README.md) + [server/shared/infra/lark/README.md](server/shared/infra/lark/README.md) |
| 碰授权源（给容器注入凭据的 ama / lark-user） | [server/core/README-integrations.md](server/core/README-integrations.md) |
| 装依赖、跑构建、排查环境 | [docs/guides/environment.md](docs/guides/environment.md) |
| 改问题反馈采什么 / 加「只给开发团队」的接口 | [server/core/README-feedback.md](server/core/README-feedback.md) |
| 碰问题（Issue）/ 附件上传与访问 | [server/core/README-issue.md](server/core/README-issue.md) |
| 碰产品建议（用户给平台提意见） | [server/core/README-suggestion.md](server/core/README-suggestion.md)（**第三种「有问题」的东西，平台级；别叫它 feedback**） |
| 碰平台自巡检（定时任务发现问题 → 自动建 issue） | [server/core/README-inspect.md](server/core/README-inspect.md) |
| 改 AMA CLI（容器里给 agent 用的 `ama` 命令，也是 `ama daemon`） | [server/cmd/ama-cli/README.md](server/cmd/ama-cli/README.md) + [cli/README.md](cli/README.md)（npm 包装） |
| 用脚本初始化方舟上的 agent | [scripts/setup-ark-agent/README.md](scripts/setup-ark-agent/README.md) |

**本机跑不起来时先翻 `docs/guides/environment.md`**——那里的坑多是「报错不指向真实原因」，
照着报错查会绕远路。

### 碰 UI 之前先读约定

**只要这次改动会影响界面长什么样**（新增页面、改布局、加组件、动配色圆角间距、
调空态与错误态、改移动端适配），动手前先读 `docs/DESIGN.md`、
[docs/guides/heroui.md](docs/guides/heroui.md) 与 [docs/guides/styling.md](docs/guides/styling.md)，
并对照 [HeroUI v3 官方文档](https://heroui.com/docs)。

这个仓库的 UI 有一整套既成约定（HeroUI 3 的复合组件形状、`onPress` 而非 `onClick`、
未分层 CSS 会赢过 `@layer` 覆盖、`Select.Value` 会渲染整个选项、`InputGroup` 的
最后一个子节点是 Trigger），**不读就会写出「能编译、能跑、但和整站不是一套」的东西**，
而那种不一致 code review 看不出来、只有截图才照得见。

## 仓库结构

```
server/           后端 Go（module: github.com/chyroc/ama/server）
  cmd/amad/       服务进程入口
  cmd/ama-cli/    ama 命令：容器里给 agent 用，也是自带机器的 daemon
  cmd/gen-openapi/ cmd/gen-authz-capabilities/ cmd/gen-doc-outline/  生成器（见「约定」）
  cmd/selfhosted-bootstrap/  手动建一个 self_hosted 方舟 Environment 的小工具
  httpd/          HTTP 骨架：路由、CORS、panic 兜底、认证与授权的装配
  apihandler/     HTTP handler
  auth/           认证：本地账号 / OIDC 登录、会话、API Key、Protect 中间件
  authz/          授权：空间成员角色 → 权限矩阵 → Guard 装饰器（新增路由要在这声明）
  core/           业务：空间 / 成员 / Agent / 技能 / 方舟凭证 / 运行历史 / worker
  service/ orchestrator/  按资源拆出来的业务服务与跨服务编排（见各自 README）
  feishu/         业务：接入飞书应用
  openapi/        OpenAPI 生成与对账
  model/apimodel/  跨模块共用的基础类型与事件定义。**进 HTTP 契约的实体在这里**
  model/persistmodel/ 只给存储层用的类型（Store 入参、不进响应的行结构体）。
                  判据是「会不会进 HTTP 响应」而不是「有没有 json tag」，见该目录 README
  repository/dao/ Store 的 PG 与内存实现
  repository/schema/ 全部 SQL/DDL，**一表一文件、文件名就是表名**，每份顶部有生成的
                  「表结构一览」。下发顺序由 schema.go 里的有序列表显式表达。加表见该目录 README
  shared/infra/   **接入层（有 io 的）**：config（YAML + 环境变量）、lark（飞书开放平台）、
                  ma（方舟 Managed Agents）、docker（Docker Engine API 客户端）、
                  dockertools（默认执行端：每个方舟会话一个容器）、
                  shelltools（六个 agent 工具本身，与执行端无关）、
                  pg / pgmq / redis（存储与队列）、s3（S3 兼容对象存储，文件字节）、oidc（OpenID Connect 登录）、
                  github（GitHub device flow 登录）、
                  cryptobox / secret（加密与凭据）、filebytes（附件字节读写）
  shared/gopkg/   **纯函数工具（无 io）**：attachment、authzdoc、humanize、mdtitle、
                  resource、selfurl、requestid、xlog
web/              前端 Vite + React 19 + TS + Tailwind 4 + HeroUI 3
cli/              ama 命令的 npm 包装（预编译二进制）
sandbox-image/    Docker 会话容器的镜像
deploy/           amad 镜像里的默认配置
docs/
  DESIGN.md       前端设计文档：信息架构、数据模型、路由、事件呈现策略
  guides/         分场景的开发指引，见上面的索引表
scripts/
  brand-icons/    品牌图 → favicon/PWA 图标 + 站内 logo 的矢量路径
  model-icons/    模型厂商标 → 内联矢量路径
  export-seed/    把前端 seed.ts 导出成 seed.json
  setup-ark-agent/  在方舟上初始化 agent 与技能的脚本
  gen-schema-outline.py  重新生成每份 .sql 顶部的「表结构一览」
```

`server/` 下每个业务模块与接入层都有自己的 README，是该包的一手资料；
`docs/guides/` 只提炼「动手前必须先知道」的部分。

## 常用命令

**整套跑起来（在仓库根）**：

```bash
./scripts/init-env.sh            # 生成 .env（口令与密钥随机生成；compose 缺任何一个都拒绝启动）
docker compose up -d --build     # PG + Redis + 对象存储（RustFS）+ 会话镜像 + amad，打开 http://localhost:9999
```

**本地开发**——一键构建前后端并启动单进程：

```bash
docker compose up -d postgres redis s3   # 只起依赖（PG 端口 15432，Redis 16379，对象存储 19000）
./dev.sh                     # build 前端 → 嵌进后端 → 重启，监听 0.0.0.0:5273
./dev.sh --skip-web          # 只改了 Go 代码，跳过前端构建
./dev.sh --stop              # 停
./dev.sh --logs              # 跟日志
```

**重复执行即重启**。前端产物用 `go:embed` 进二进制（`server/httpd/static.go`），
所以本地只有一个进程、一个端口、天然同源——**没有 HMR，前端改动必须重跑 `./dev.sh`**。

⚠️ **「一个进程」只是本地的样子，生产是多实例**（见上面「这是分布式服务」）。
本地单进程跑通不能作为「实现正确」的证据——进程内状态在这里永远是对的。

⚠️ **`dev.sh` 起新进程前会按端口清理旧进程**（先 `kill`，0.5 秒后还在就 `kill -9`），
它不看那个进程是谁的。同一台机器上跑多份（比如多个 git worktree）时每份用不同端口，
`--stop` 也要带同一个 `PORT=`：

```bash
PORT=5301 ./dev.sh --skip-web
PORT=5301 ./dev.sh --stop
```

`PORT=` 留空等于没写（`${PORT:-5273}` 对空串也会回落到 5273）。

后端（在 `server/` 下）：

```bash
go build ./...
go vet ./...
gofmt -l .                  # 输出为空即格式正确
go test -race ./...         # 无需凭证
```

⚠️ **碰库的测试连不上 PG 时整组 `t.Skip`，跳过时是绿的。** 改了 DDL 或 `*_pg.go` 的 SQL，
要起一个 PG 再跑一遍才算验过。默认连 `127.0.0.1:15432`（库/用户是 `ama`，即
`docker compose up -d postgres`）。⚠️ compose 的 PG 口令是 `.env` 里随机生成的，**要带上
`AMA_TEST_PG_PASSWORD=$(grep ^AMA_PG_PASSWORD= .env | cut -d= -f2-)`**，否则认证失败、整组被跳过且显示为绿。可用 `AMA_TEST_PG_HOST` / `AMA_TEST_PG_PORT` /
`AMA_TEST_PG_USER` / `AMA_TEST_PG_PASSWORD` / `AMA_TEST_PG_DATABASE` 指到别处。
测试会建临时 schema 并灌全部 DDL，**别指向有真实数据的库**。

前端（在 `web/` 下）：

```bash
npm install
npm run build                # check-icons && tsc -b && vite build
npm run typecheck
npm run check-icons          # 图标对账：用到的都存在且已注册（离线可用）
npm run e2e                  # Playwright，用系统 Chrome
./scripts/dev.sh             # 单独起 vite（有 HMR，但不含后端；日常用根目录 dev.sh）
```

## 架构要点

**数据落 PostgreSQL**——`core.Store` 与 `feishu.Store` 都有 PG 实现，默认走它
（`AMA_STORE=mem` 可回退到内存，只适合本地单进程）。空库才装种子，重启不冲掉数据。

**文件字节在对象存储，元信息在 PG。** 附件、技能文件、技能包、个人文件的字节只存 S3 兼容
对象存储（`shared/infra/s3`，compose 里是 RustFS），PG 只存桶 + key 这类元信息，**不存任何字节**。
PG 档没配 `s3.bucket` 时 amad 拒绝启动，不会退回去把字节写进库。
⚠️ **预签名链接把 Host 签进去了**，同一条链接只有签名时那个地址打得通：浏览器 / 用户自带机器用
`s3.publicEndpoint`，Docker 会话容器用 `s3.sandboxEndpoint`，amad 自己读写用 `s3.endpoint`。
签错的症状不指向配置（浏览器下载 403、容器里技能只剩 SKILL.md），见 `server/shared/infra/s3/README.md`。

**对话跑在 self_hosted 上，工具在我们自己的执行端里执行**——对话页那三个接口
（建会话 / 发事件 / SSE 订阅）驱动 Managed Agents，环境类型是 `self_hosted`：
方舟只做模型推理，工具调用扔进 work 队列，由 amad 里常驻的 worker 领走、下发到
执行端，再用 `user.tool_result` 事件把结果交回方舟。

**禁止使用 `cloud` 环境**——那样工具跑在方舟的托管沙箱里，执行环境不在我们手上。
`EnsureEnvironment` 建完会回读校验类型，不是 self_hosted 直接报错。

代码：`core/chat.go`（会话）+ `core/worker.go`（worker 池）
+ `shared/infra/dockertools/`（Docker 执行端）+ `shared/infra/shelltools/`（六个工具）
+ `service/host/`（自带机器执行端）。坑记在 `server/core/README.md`。

**六个工具（bash/read/write/edit/glob/grep）全部落进同一个执行环境**，共享同一个文件系统。
只换 bash 会让「bash 写的文件 read 读不到」——那个不一致不报错，只会让 agent 反复重试。
Docker 执行端的工作区是每个会话一个的命名 volume，容器被删了自动重建、文件还在。

**容器第一次真调工具时才建，一轮结束后空闲就暂停**——`end_turn` 后空闲满 SDK 的
`MaxIdle`（默认 60 秒）按 `docker.idleAction` 处理（默认 `pause`，可选 `stop` / `none`），
下一条消息真调到工具时再恢复，纯文本回复不会碰容器。容器名由会话 id 算出来，多实例抢建
靠 daemon 的 409 裁决；会话与容器的对应记在 PG（`run_sandbox_sessions`）。回收巡检抢 Redis
租约，闲置超过 `containerTTL`（24h）删容器、超过 `workspaceTTL`（7d）删工作区。

worker 要能连上 Docker daemon（默认 `unix:///var/run/docker.sock`，或 `DOCKER_HOST` /
`docker.host`），会话镜像默认 `ama-sandbox:latest`（`sandbox-image/Dockerfile`）。
连不上时服务照常起，但**agent 一调工具就报错**；启动日志里会明说 Docker 执行端是否装配成功。
⚠️ **能访问 docker.sock 等于宿主机 root**，部署与隔离见
[docs/guides/docker-executor.md](docs/guides/docker-executor.md)。

**配置**：一份 `config.Config` 收齐全部配置（pg / redis / server / auth / apiKey / ark / docker /
worker），来源只有一份 YAML 文件（`AMA_CONFIG` 指定，默认 `config.yaml`）加环境变量，
优先级是 **环境变量 > 文件 > 默认值**。`cmd/amad` 里没有任何 `os.Getenv` 业务配置。
见 [server/shared/infra/config/README.md](server/shared/infra/config/README.md)。

**登录是「本地账号 + 通用 OIDC + GitHub」三种**，由配置 `auth.local` / `auth.oidc` / `auth.github` 决定开不开
（`auth` 包 + `shared/infra/oidc`）：

- 本地账号：`POST /api/auth/login` / `POST /api/auth/signup`，bcrypt 存哈希（`auth_local_accounts`）。
  `auth.local.allowSignup=false` 时**仍允许注册第一个账号**（库里一个用户都没有时，它成为平台管理员），
  之后由平台管理员 `POST /api/admin/users` 建。密码登录按邮箱限流，计数在 PG（`auth_login_failures`）。
- OIDC：`GET /api/auth/oidc/login` → IdP → `GET /api/auth/oidc/callback`，Authorization Code + PKCE +
  state + nonce，三样都落 PG（`auth_oidc_states`，回调可能落到另一个实例）。邮箱取 ID token 的 `email`，
  默认要求 `email_verified`。
- GitHub：OAuth device flow（`POST /api/auth/github/device` → 用户去 github.com 输码 → 浏览器轮询
  `POST /api/auth/github/poll`），只要 client id（不配时默认用 GitHub CLI 的公开 client id，见 `github.DefaultClientID`）。device_code 与轮询间隔落 PG（`auth_github_flows`），
  flow id 还要配一把 HttpOnly 绑定 Cookie 才能轮询；**准入白名单全空时谁都进不来**。见 `auth/github.go`。
- `GET /api/auth/providers` 告诉前端开了哪些方式。两种都不开时启动日志 ERROR，人登不进网页。

**小写邮箱就是用户唯一 ID（Actor）**，除 `/api/auth/` 下的登录接口与 `/healthz` 外全部接口要求已登录。
凭据是随机 token 进 HttpOnly Cookie（库里只存 sha256，所以登出是真的能吊销），
身份走 `context` 不走请求头（请求头是调用方能改的，context 只有服务端写得进去）。
e2e / CI / 脚本走固定 API Key（`AMA_API_KEY` + `AMA_API_KEY_EMAIL`，两个都配齐才启用，
生产环境不要配）。用户也可以在设置里建自己的 API Key（`amak_` 前缀），权限严格继承本人。
⚠️ API Key 进 `Authorization` 头，**别写进命令行参数**——`ama` CLI 刻意没有 `--token` 参数，
因为命令行会原样落进 `RunEvent` 永久记录。

**授权**：`authz` 包按**空间成员角色**判权限——owner/admin 全部、member 读写、viewer 只读、
**非成员一律 403**，空间列表只返回自己所在的空间。⚠️ **新增 HTTP 路由必须在
`server/authz/matrix.go` 里声明权限，否则启动 panic**（漏声明会静默放行，那是零报错的故障）。
三条系统驱动路径（worker 池巡检、飞书入站、定时任务）**没有身份、刻意绕过授权**，
别给它们加鉴权——加了会让机器人静默不回话、工具调用卡死。动手前读
[server/authz/README.md](server/authz/README.md)。

**对话页**（`/w/:key/chat`）复用 Run/RunEvent：一次对话就是一条 `trigger: "manual"` 的
运行，所以它自动进运行历史、能从历史续聊。实时性走方舟 SSE 的 `event_deltas` 逐字增量，
增量帧刻意不进 `RunEvent` 联合（方舟明确它不落 event history）。见 `docs/DESIGN.md`。

## 建表：字段展开成列，一对多用关联表

**默认展开成列，不要塞 `doc` / `extra` 这类 jsonb。** 判定问句只有两句：

> **一、你能把这个东西的字段列全吗？**（按顺序判，先命中先算）
>
> 1. **是一对多吗？** → **建关联表**，到此为止。**PG 无法对 jsonb 数组元素建唯一约束**，
>    放 doc 数组里连去重都做不到。⚠️ 这条优先于下面全部，
>    包括「键由用户定」——用户自定义的环境变量/标签**形状是固定的**（key、value 两列），
>    用户定的是值域不是形状，所以是关联表 + `unique(owner, key)`，不是 jsonb。
> 2. **能列全** → 建列。字段有多少个就建多少列，没有上界；「字段多」「以后可能加字段」
>    都不是理由（加列可以和代码同一版走，不对称的只有删列）。
> 3. **列不全，且形状由别人定**（第三方回传的原始结构、用户自定义的键值）→ 才用 jsonb。
>    ⚠️ **「逐字节保真」要的是 `text`/`bytea`**——jsonb 会重排键、压空白，恰恰不保真
>    （先例：`run_events.raw`）。
>
> **二、（想用 jsonb 时再问一遍）有没有任何一个查询要按这里面的某个字段过滤、排序、
> 分组或去重？** 有，哪怕只有一个 → **那个字段必须是列**。全都没有、且它是一整块
> 原样存取的载荷（不定条数的明细、一帧要回放的快照）→ 才可以整块进 jsonb，
> 且**这张表剩下的字段仍然都要是真列**。

⚠️ **「进 HTTP 契约」不是用 doc 的理由。** `issues` / `suggestions` 两个域全是真列、
一个 doc 都没有，而它们同样进契约。

⚠️ **给已有 doc 的表加字段也要加成列**，不要往 `agents.doc` / `runs.doc` 里再塞一个键
（那是零 DDL、零报错的默认路径，规则最容易在这里失守）。
**列漏一个是编译错误，doc 漏一个是几周后才发现的数据丢失。**

⚠️ **这条有机器闸门**：新增 `doc jsonb` 列或往白名单外的表加 jsonb，
`go test ./repository/schema/` 直接红，报错里带判据。

目前 52 张表里有 9 张带 doc 列（历史原因，不强制改，迁移时往全列方向收），新表必须遵守。
完整规则与姿势见 [server/repository/schema/README.md](server/repository/schema/README.md)。

## 时间戳：单位与格式

**本项目自己产生的时间戳，一律是 ISO 8601 字符串，固定三位小数（毫秒），UTC，
字面 Z 结尾**——`2026-07-26T10:00:00.000Z`。生成用 `apimodel.NowISO()`。

### 列类型按用途二选一，不要凭感觉

| 用途 | 列类型 | 先例 |
| --- | --- | --- |
| **进 HTTP 契约**、要按字符串排序的 | `text`（不透明 ISO 串） | `runs.started_at`、`issues.created_at` |
| **不进契约**、给数据库比较/排序用的 | `timestamptz` | `schedules.next_run_at`、`workspaces.created_at`、`auth_sessions.created_at` |

判据是「前端会不会拿这个值做字符串比较」。会，就必须是 text 且定长三位小数；
不会（只是给 `order by` / `where` 用），就用 `timestamptz`，让数据库当唯一时钟。

**同一个概念可以两边都有**：`workspaces` 的 `created_at` 列是 `timestamptz`（排序用），
而 `doc` 里的 `createdAt` 仍是 ISO 串（契约用），两者由同一次写入一起落库。

### 别用 `time.RFC3339`

`time.RFC3339` 与 `RFC3339Nano` **都会把 `.000` 抹掉**，于是同一时刻在库里出现两种
写法。这不是好看不好看的问题：`listRuns` 按 `startedAt` 做**字符串**降序排，
而 `.`(0x2E) < `Z`(0x5A)，两种形状混在一起会**直接改变排序结果**。

### ⚠️ 单位混淆：外部来的时间戳必须逐个确认

本项目内部只有一种单位（毫秒 ISO 串），**但对接的外部系统不统一，同一家的两个接口
就可能不一样**：JWT 的 `exp` / `iat` 是秒（规范如此），而不少开放平台接口回的
`expire_at` 是毫秒。把毫秒当秒解析会算出几万年后的过期时间，于是 token「永不过期」——
然后在真正过期时静默 401，而代码里那个「还早着呢」的判断一次都不会触发。

**只能查文档，不能靠猜。** 判据很简单：把数字当秒解析，如果得到一个离现在很远的年份
（几万年后），那它其实是毫秒。

## 加了「要配置才生效」的代码，要一起交付到配置为止

新增一个配置项时，同一个改动里要把 `server/config.example.yaml`（逐项注释）、
`server/shared/infra/config` 的环境变量映射、需要的话 `.env.example` 与 `docker-compose.yml`
一起补上。只提交读配置的代码等于交付了一个不生效的功能，而这种半成品有三个共同点：

1. **零报错**——服务照常起、健康检查照常绿，只有那个能力静默不可用；
2. **报错不指向配置**——症状出现在最远端（比如「容器里跑 `ama` 说没有凭据」），
   那句话指向 CLI，不指向「配置里少一个键」；
3. **交接即失传**——你知道差一步，下一个人只看到「代码里明明写了、就是不好用」。

验证时挑**能证伪的**信号：

- **配置只在启动时读。** 滚动发布期间「配置到达之前起来的实例」照旧报缺配置，
  判据是「配置送达之后才起来的实例还报不报」。
- **别信只判对象非 nil 的日志。** 依赖注入里那个对象总是非 nil，于是启动日志说
  「已装配」而功能其实不可用。改成问「这个能力自己报告可用吗」（`XxxEnabled()`），
  别问「指针在不在」。

## 约定

**用词要直白，不要用「高级」的词。** 包名、类型名、函数名、注释、文档、写给用户
的结论，一律选那个最平常的说法。

判据是**「一个刚接手这个仓库的人，不查词典、不问人，能不能一眼看懂」**。看不懂就换。

这不是文风偏好。抽象名词的问题是**它看起来像是解释了，其实没有**：

- `contract` / `entity` / `descriptor` / `provisioning` / `orchestration` 这类词，
  读的人得先猜它在这个仓库里具体指什么，而猜错了不会有任何东西报错——
  只会让下一个人往里放错东西。`model/apimodel`（给 HTTP 用的）比
  `model/contract`（"契约"？谁和谁的契约？）多一次都不用猜。
- 包名尤其要小心：它会出现在**每一个调用点**（`apimodel.Agent` 要被写几千次），
  选错的代价按引用数放大，而改名要动几千处。
- 中文同理：**别写「收敛」「对齐」「语义」「范式」「幂等性设计」这类词**去描述
  一件具体的事。说「把三处改成同一种写法」，不说「收敛实现」；说「两边给出同一个
  答案」，不说「语义对齐」。

⚠️ **例外只有一种：这个词本身就是外部系统的既有名字。** `OAuth`、`jsonb`、
`timestamptz`、`ArkBinding`、`SkillsFingerprint`、`idempotency key` 照原样用——
换成"自己的说法"反而查不到文档、对不上上游。判据是「能不能在官方文档里搜到它」。

**Go**：标准 `gofmt`。**代码注释一律用英文**（所有语言：Go / TS / SQL / shell / YAML 等），commit message 也用英文（见「提交与 Pull Request」）。日志、报错与界面文案不是注释，按现有写法保留。

**前端**：动手写 `web/src` 前先读 `docs/DESIGN.md`。`@` 别名指向 `web/src`。
TypeScript 严格模式，且开了 `noUnusedLocals` / `noUnusedParameters`——
未使用的变量会直接编译报错。

**改了 HTTP 接口就要重新生成 OpenAPI，且这件事有机器管。**

```bash
cd server && go run ./cmd/gen-openapi     # 重新生成 openapi/openapi.json
```

`GET /api/openapi.json` 的内容**从 `authz/matrix.go` 的路由权限表生成**，不是手写的
（手写必然漂移，而 agent 拿着过时的 spec 会反复调错接口、且报错不指向真实原因）。
产物 `server/openapi/openapi.json` 入库，所以要跟着提交。

忘了跑会怎样：**`go test ./openapi/` 直接红**，报错里带修复命令。整条闭环是

```
新增路由 → 必须写 matrix（漏了服务启动 panic）
        → spec 自动跟着变（从 matrix 生成）
        → 对账保证 spec 已重新生成并提交（TestSpecFileIsUpToDate）
```

**同一张 matrix 还生成给人看的「角色权限说明表」**（成员页底部那个折叠区）：

```bash
cd server && go run ./cmd/gen-authz-capabilities   # 产物 web/src/data/roleCapabilities.ts
```

理由相同——手写的权限说明必然漂移，而**过时的权限说明比没有说明更坏**
（用户照着它以为自己能做某事，实际被 403）。
漏了归类或忘了重新生成：**`go test ./shared/gopkg/authzdoc/` 直接红**，报错带修复命令。
⚠️ **表里 owner 与 admin 必然完全相同**（`levelOf` 里两者同级），给 owner 多打勾是错的。
见 [server/authz/README.md](server/authz/README.md)。

**临时文件别放仓库里**：探针脚本、临时 config 放 `/tmp`；vite/playwright 的 config
必须在包目录里才解析得到依赖的，用完立刻删。

## 提交与 Pull Request

- 从 `main` 拉分支改，提 GitHub Pull Request，别直接推 `main`。
- **commit message 必须用英文写**（标题与正文都是），用 `type(scope): summary` 开头
  （如 `fix(feishu): ...`、`feat(web): ...`）。这是公开仓库，提交历史要让不读中文的贡献者
  也看得懂；代码注释同样用英文，`docs/` 与各 README 文档目前仍是中文。
- **踩过的坑写进 commit body 或就近的 README / `docs/guides/` 下对应的场景文档**，
  不要只留在 PR 讨论里。
- 提 PR 前本地跑过：后端 `gofmt -l .`（为空）+ `go build ./...` + `go vet ./...` +
  `go test -race ./...`（碰了 SQL 就带 PG 跑）；前端 `npm run typecheck` + `npm run build`。
  改了路由就带上重新生成的 `openapi.json` 与 `roleCapabilities.ts`。
- PR 描述写清改了什么、为什么、怎么验证的。
