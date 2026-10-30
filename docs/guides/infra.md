# 外部接入层

**读它的时机**：碰 `server/shared/infra/` 下表里的任何一个包。一手资料是各包的 README（或包注释），
**这里只放「动手前必须先知道、否则会绕远路」的那部分**，细节去读各自的 README。

| 包 | 接的是什么 | 一手资料 |
| --- | --- | --- |
| `shared/infra/lark/` | 飞书开放平台：设备码注册 + 应用巡检 | `server/shared/infra/lark/README.md` |
| `shared/infra/ma/` | 方舟 Managed Agents | `server/shared/infra/ma/README.md` |
| `shared/infra/docker/` | Docker Engine API（net/http 直连 unix socket / `DOCKER_HOST`，不支持 TLS） | 包注释 |
| `shared/infra/dockertools/` | 默认执行端：每个方舟会话一个容器 | `server/shared/infra/dockertools/README.md` |
| `shared/infra/shelltools/` | 六个 agent 工具本身（与执行端无关） | 包注释 |
| `shared/infra/pg/` | PostgreSQL：托管实例 / 本地容器 | `server/shared/infra/pg/README.md` |

本地凭据放仓库根的 `.env`（已 gitignore），跑 example 前
`set -a; source .env; set +a`。详见 [environment.md](environment.md)。

## 飞书（`shared/infra/lark`）

**协议状态带 HTTP 400。** `action=poll` 在「还在等用户授权」时返回 **HTTP 400** +
`{"error":"authorization_pending","code":20094}`，设备码非法同样是 400。按状态码判成败
会把「还在等」当成请求失败，**整条轮询链路直接断掉**。

**这个坑离线单测抓不到**（假服务器默认回 200），是真连了一次飞书才暴露的——现在假飞书
照抄真实状态码。另外 `invalid_grant`（20079）必须单独当终态，别掉进「未知错误」去尝试
换账号域重试。详见 `server/shared/infra/lark/README.md`。

## 方舟（`shared/infra/ma`）

**`self_hosted` 的 Environment 不能带 `config.networking`。** 传了会 400
（`config.networking: not allowed when config.type is "self_hosted"`）——出网由自建运行
环境自己管。`EnvironmentSpec` 原先无条件填 networking 默认值，导致 `EnvSelfHosted` 走
便捷入参**永远建不出来**。现在只有 `EnvCloud` 才填。文档没写这条，是真连方舟建环境
才暴露的。

**`ma.WithRegion` 对 BytePlus 无效。** SDK 的 `NewClientWithApiKey` 硬编码火山的
`volces.com`，`WithRegion` 只改 region 字段不改域名。

扩展该包时沿用其 README「设计取舍」一节的约定：一个文件只放一类 API；便捷入参配
`*Raw` 逃生口；返回值直接用 SDK 原生 struct，不做二次 DTO 映射。

## Docker 执行端（`shared/infra/docker` + `dockertools`）

**每个方舟会话一个容器 + 一个工作区 volume**，六个工具都经 `docker exec` 落进同一个容器。
容器第一次真调工具时才建，空闲后按 `docker.idleAction` 暂停 / 停止；容器被删了下次调用自动
重建，volume 还在就不丢文件。部署、配置与安全（**docker.sock 等于宿主机 root**）见
[docker-executor.md](docker-executor.md)，状态机与多实例见该包 README。

## 方舟 self_hosted 环境的 worker 协议

现在走 SDK 的 `EnvironmentWorker`（`core/worker.go`），下面几条协议事实是它底下的行为。

**self_hosted 是拉模型。** 方舟不连你的容器，`EnvConfig` 里也没有填地址的字段；
是 worker 去长轮询 `/work/poll` 拉活、`ack` 认领、`heartbeat` 续租。work 的 id 就等于
session_id。这 8 个接口**方舟 SDK 一个都没有**，所以是手写 HTTP。

**鉴权就一把 `ARK_API_KEY`。** 自建 Worker 那 8 篇文档的正文里写着「建议使用
Environment Key 鉴权（在 Console 环境详情页生成，仅在生成时展示一次）」，据此曾以为
worker 侧需要一把单独的凭据，还围着它写了双 key 分流和「只能人工去控制台点」的结论。
**实际上没有 Environment Key 这个东西**：8 篇的「鉴权」小节列的都只有「API Key 鉴权」。
教训是——**方舟文档的凭据以每篇的「鉴权」小节为准，别信正文里的顺口一提。**

**work payload 里没有凭据。** 文档说「携带本次执行所需凭据 payload」，实测只是个
session 指针 `{"id":"sesn-...","type":"session"}`。真正的 model / system / tools 要拿这个
id 去查 Session 详情。

**得先有 Session 派到该环境**才会有 work 可拉，否则 worker 空转，看起来像「连不上」
其实队列里没活。

`cmd/selfhosted-bootstrap` 可以单独建一个 self_hosted 的 Environment（要 `ARK_API_KEY`）。

## PostgreSQL（`shared/infra/pg`）

标准 PostgreSQL：`host:port` + 用户名密码，走 `pgx/v5/stdlib`。生产可以用任意托管 PG，
本地是 `docker compose up -d` 起的 `postgres:17-alpine`，**同一条代码路径**。
跑法见 [environment.md](environment.md)。

**IPv6 在 key=value DSN 里不能加方括号。** 不少文档说「IPv6 必须用方括号包裹」，
那条**只适用于 URI 形式**；libpq 的 key=value 形式里方括号会被当成主机名的一部分
（实测 `host=[::1]` → `lookup [::1]: no such host`，而 `host=::1` 正常）。
**照文档抄反而错**，而且 `pgx.ParseConfig` 两种都不报错，解析阶段发现不了。

**DSN 里 `sslmode` 不能写成 `ssl_mode`。** 写错**不报错**：pgx 不认识这个键，会把它当成
PostgreSQL 的未知启动参数原样发出去，于是 sslmode 实际没生效而你以为设了。
`standard.go` 的 `misspelledKeys` 对这几个易错写法做了显式拦截。

**托管 PG 通常不给超级用户。** 账号要有建表权限（并设成库的 Owner），所以本包的起点是
「一个已经存在的库 + 一个已经有权限的账号」。
