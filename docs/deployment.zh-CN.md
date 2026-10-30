# 部署

[English](deployment.md) | 中文

## 用 docker compose 部署

需要：Docker（含 compose 插件）、一个火山方舟账号的 API Key。

```bash
./scripts/init-env.sh    # 生成 .env：PG / Redis / 对象存储口令与两把加密密钥都填随机值
docker compose up -d --build
docker compose ps        # 等 postgres / redis / s3 healthy、amad 起来
```

然后：

1. 打开 <http://localhost:9999>，注册第一个账号——它自动成为平台管理员。之后默认关闭自助注册，
   新账号由平台管理员调 `POST /api/admin/users` 建；要开放注册就在 `.env` 里设
   `AMA_AUTH_LOCAL_ALLOW_SIGNUP=true` 并 `docker compose up -d`。
2. 在空间设置的「方舟凭证」里填方舟 API Key（也可以填一把账号级凭证，各空间沿用）。
   self_hosted Environment 由 amad 第一次建会话时自动创建，不用手动建。
3. 建一个 agent，开始对话。agent 第一次调用工具时，amad 才会经 Docker 起它的会话容器
   （镜像 `ama-sandbox:latest`，compose 已用 `sandbox-image/Dockerfile` 构建好）。

compose 起了五个服务：`postgres`（宿主机端口 15432）、`redis`（16379）、
`s3`（RustFS，`${AMA_S3_PORT:-19000}`）、`sandbox-image`（只负责构建会话镜像，构建完即退出）、
`amad`（`${AMA_PORT:-9999}`）。

**端口与口令**：

- amad 与 s3 对所有网卡开放（浏览器要连）；PG 与 Redis **只绑 127.0.0.1**，只给本机的
  `dev.sh` 与调试用，amad 走 compose 内网连它们。
- 六个口令/密钥（`AMA_PG_PASSWORD`、`AMA_REDIS_PASSWORD`、`AMA_S3_ACCESS_KEY`、
  `AMA_S3_SECRET_KEY`、`ARK_SECRET_KEY`、`AMA_AUTH_MANAGED_KEY_SECRET`）**都必须在 `.env` 里有值**，
  缺一个 compose 就直接报错不起——不存在「忘了改默认口令」这回事。
- ⚠️ PG 口令只在第一次建数据卷时生效。数据卷已存在时改 `AMA_PG_PASSWORD`，库里的口令不会变，
  amad 会连不上；要么进库 `ALTER USER ama PASSWORD '...'`，要么 `docker compose down -v` 重建（会清数据）。
- 从别的机器访问时，把 `AMA_PUBLIC_URL`、`AMA_TERMINAL_WS_ORIGINS`、`AMA_S3_PUBLIC_ENDPOINT`
  改成实际地址（默认都是 localhost）。

⚠️ **对象存储的预签名链接把 Host 签进去了**，同一条链接只能用签名时那个地址打开。
个人文件的下载链接是签给浏览器的（`AMA_S3_PUBLIC_ENDPOINT`，默认 `http://localhost:19000`），
浏览器不在本机时要改成 `http://<本机 IP 或域名>:19000`，否则点下载打不开。
会话容器拉技能包用的是另一个地址（容器网络里的 `http://ama-s3:9000`），两边互不替代。
issue / 建议的附件不受影响（它们经 amad 中转）。细节见 `server/config.example.yaml` 的 `s3` 段。

⚠️ `AMA_PUBLIC_URL` 同时会注入成会话容器里 `ama` 命令的 `AMA_BASE_URL`。默认的
`http://localhost:9999` 在容器里指向容器自己，所以容器内的 `ama` 命令连不上 amad。
要让 agent 在容器里用 `ama`，把 `AMA_PUBLIC_URL` 配成一个浏览器和会话容器都能访问的地址
（比如反向代理后的域名，或宿主机局域网 IP）。

## ⚠️ Docker 执行端的安全提醒

compose 把宿主机的 `/var/run/docker.sock` 挂进了 amad。**能访问 docker.sock 等于拥有宿主机 root**
（能起特权容器、挂载宿主机根目录）。所以：

- amad 本身必须可信，别把它（尤其是固定 API Key、`AMA_DEV_ROUTES` 打开的调试路由）暴露给不可信的人；
- 会话容器拿不到这个 socket，但它们与宿主机共享内核，容器隔离不等于虚拟机隔离；
- 要更强的隔离：daemon 配 rootless / userns-remap，或用 gVisor（runsc）之类的运行时，
  或用 `AMA_DOCKER_HOST` 让 amad 指向一台专门跑会话容器的远端 daemon。

细节见 [docs/guides/docker-executor.md](guides/docker-executor.md)。
自带机器执行端见 [docs/guides/host-executor.md](guides/host-executor.md)。

## 配置

配置来源只有一份 YAML 文件（路径由 `AMA_CONFIG` 指定）加环境变量，优先级 **环境变量 > 文件 > 默认值**。

- [`server/config.example.yaml`](../server/config.example.yaml)：全部配置项与逐项说明，本地开发复制成 `server/config.yaml`。
- [`deploy/config.yaml`](../deploy/config.yaml)：镜像里的默认配置，只写了 PG 连接，其余走环境变量。
- [`.env.example`](../.env.example)：compose 与 `dev.sh` 读的环境变量模板。
- 环境变量与字段的对应关系见 [`server/shared/infra/config/README.md`](../server/shared/infra/config/README.md)。

常用环境变量：

| 变量 | 作用 |
| --- | --- |
| `AMA_PG_HOST` / `AMA_PG_PORT` / `AMA_PG_DATABASE` / `AMA_PG_USER` / `AMA_PG_PASSWORD` / `AMA_PG_SSLMODE` | PostgreSQL 连接 |
| `AMA_REDIS_ADDR` / `AMA_REDIS_PASSWORD` | Redis（留空则不启用飞书长连接） |
| `AMA_S3_ENDPOINT` / `AMA_S3_PUBLIC_ENDPOINT` / `AMA_S3_SANDBOX_ENDPOINT` | 对象存储地址：amad 读写 / 签给浏览器与用户机器 / 签给会话容器（后两个留空同 endpoint） |
| `AMA_S3_BUCKET` / `AMA_S3_REGION` / `AMA_S3_ACCESS_KEY` / `AMA_S3_SECRET_KEY` | 对象存储桶与凭据（PG 档必配，没配拒绝启动） |
| `AMA_S3_USE_PATH_STYLE` / `AMA_S3_CREATE_BUCKET` | path-style 寻址、启动时自动建桶（默认都是 true） |
| `AMA_PUBLIC_URL` | 对外地址：Cookie 的 Secure、OIDC 回调、容器里 `ama` 的 `AMA_BASE_URL` |
| `AMA_TERMINAL_WS_ORIGINS` | 允许开网页终端 WebSocket 的页面来源 |
| `ARK_SECRET_KEY` | 加密方舟凭证的主密钥（换了它旧密文解不开） |
| `AMA_AUTH_LOCAL_ENABLED` / `AMA_AUTH_LOCAL_ALLOW_SIGNUP` | 本地账号登录 / 自助注册 |
| `AMA_AUTH_OIDC_*` / `AMA_AUTH_ADMINS` | OIDC 登录 / 平台管理员 |
| `AMA_AUTH_GITHUB_*` | GitHub 登录（device flow），见下 |
| `AMA_DOCKER_HOST` / `AMA_DOCKER_IMAGE` / `AMA_DOCKER_NETWORK` / `AMA_DOCKER_IDLE_ACTION` / `AMA_DOCKER_MEMORY` / `AMA_DOCKER_CPUS` | Docker 执行端 |

### OIDC 登录

任何支持 Authorization Code + PKCE 的 OpenID Connect 提供方都可以（Keycloak、Authentik、
Google、Azure AD 等）。在 IdP 上登记的 redirect_uri 是 `<AMA_PUBLIC_URL>/api/auth/oidc/callback`：

```bash
# .env
AMA_PUBLIC_URL=https://ama.example.com
AMA_AUTH_OIDC_ISSUER=https://idp.example.com/realms/main
AMA_AUTH_OIDC_CLIENT_ID=ama
AMA_AUTH_OIDC_CLIENT_SECRET=...        # 公开客户端可留空，只靠 PKCE
AMA_AUTH_OIDC_DISPLAY_NAME=公司 SSO    # 登录页按钮：「使用 公司 SSO 登录」
AMA_AUTH_ADMINS=alice@example.com      # OIDC 用户要当平台管理员只能写在这里
```

用户唯一 ID 是**小写邮箱**，本地账号与 OIDC 拿到同一个邮箱就是同一个人。默认要求 ID token 的
`email_verified=true`；只有确认邮箱由管理员维护时才打开 `auth.oidc.allowUnverifiedEmail`，
否则任何人都能填别人的邮箱冒充成员。

改完 `.env` 后 `docker compose up -d` 让 amad 重建即可生效（配置只在启动时读）。

### GitHub 登录

走 GitHub 的 OAuth device flow：登录页点「使用 GitHub 登录」，页面上出现一个短码，
用户去 `github.com/login/device` 输入并点授权，页面自动完成登录。**只要 client id，
不要 client secret，也不需要在 GitHub 上登记回调地址**。

```bash
# .env
AMA_AUTH_GITHUB_ENABLED=true
AMA_AUTH_GITHUB_ALLOWED_USERS=alice,bob       # 准入白名单，见下
AMA_AUTH_GITHUB_ALLOWED_EMAIL_DOMAINS=example.com
AMA_AUTH_GITHUB_ALLOWED_ORGS=acme             # 会自动多申请 read:org
AMA_AUTH_GITHUB_CLIENT_ID=Ov23li...           # 可选，建议换成自己的 OAuth App，见下
AMA_AUTH_GITHUB_PROXY=http://172.17.0.1:8080  # 可选：只给 github.com / api.github.com 用的代理
```

**client id 默认可用、建议替换**。不配时用 GitHub CLI 公开的 client id `178c6fc778ccc68e1d6a`
（`github.com/cli/cli` 源码 `internal/authflow/flow.go` 里的那个；device flow 不需要 secret，
所以它能直接用），开箱即可登录。代价是：用户在 GitHub 授权页上看到的是「GitHub CLI」在申请
权限（容易误导人，也让 device code 钓鱼更有迷惑性），而且对方随时可以吊销它，那时 GitHub
登录整个不可用。启动日志会对此打一条 WARN。

正式部署建议建一个自己的：GitHub → Settings → Developer settings → OAuth Apps → New OAuth App，
勾上「Enable Device Flow」，Homepage / Callback URL 填本站地址即可（device flow 用不到回调），
把 client id 填进 `AMA_AUTH_GITHUB_CLIENT_ID`。

**准入默认拒绝**：`ALLOWED_USERS`（GitHub 用户名）/ `ALLOWED_EMAIL_DOMAINS`（主邮箱域名）/
`ALLOWED_ORGS`（组织正式成员）**三项全空时任何 GitHub 账号都登不进来**，配了任意一项即
「命中任一放行」。GitHub 账号人人可注册，不设准入等于把平台开放给全世界。

邮箱取 GitHub 上 **primary 且 verified** 的那个，小写后就是 Actor——与本地账号 / OIDC 同邮箱
即同一个人。没有已验证的主邮箱会被拒绝。GitHub 的 access token 只用来读一次资料和邮箱，
不落库；它会留在用户 GitHub 的「Authorized OAuth Apps」里，用户可以自己撤销。

