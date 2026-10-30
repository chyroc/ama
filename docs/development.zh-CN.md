# 开发

[English](development.md) | 中文

## 本地跑起来

```bash
./scripts/init-env.sh                     # 没有 .env 时先生成；dev.sh 会读它拿到口令
docker compose up -d postgres redis s3    # 只起依赖（s3 会顺带建出 ama-net 网络）
cp server/config.example.yaml server/config.yaml
cd web && npm install && cd ..
./dev.sh                                  # 构建前端 → 嵌进后端 → 启动，监听 0.0.0.0:5273
./dev.sh --skip-web                       # 只改了 Go 代码时
./dev.sh --logs / --stop
```

前端产物用 `go:embed` 进二进制，所以只有一个进程、一个端口、没有 HMR，前端改动要重跑 `./dev.sh`。
⚠️ `dev.sh` 启动前会清掉占着端口的进程，不管是谁的。同一台机器跑多份（比如多个 git worktree）时
每份用不同的 `PORT=`，`--stop` 也要带同一个 `PORT=`。

本地跑 Docker 执行端要先构建会话镜像：`docker compose build sandbox-image`。

后端（在 `server/` 下）：

```bash
gofmt -l .
go build ./... && go vet ./...
go test -race ./...    # 连不上 PG 的用例会 skip；用 AMA_TEST_PG_HOST / AMA_TEST_PG_PORT 指向一个空库
```

⚠️ 被 skip 的 PG 用例显示为绿。改了 DDL 或 `*_pg.go` 里的 SQL，要连真 PG 再跑一遍。
compose 的 PG 口令是随机生成的，要带上
`AMA_TEST_PG_PASSWORD=$(grep ^AMA_PG_PASSWORD= .env | cut -d= -f2-)`。

前端（在 `web/` 下）：

```bash
npm run typecheck
npm run build
npm run e2e        # Playwright，用系统 Chrome
```

改了 HTTP 路由要重新生成 OpenAPI 与角色权限说明表（`go test` 会检查）：

```bash
cd server && go run ./cmd/gen-openapi && go run ./cmd/gen-authz-capabilities
```

## 接着读什么

- [CLAUDE.md](../CLAUDE.md)：每次改动都要遵守的约定（按多实例设计、建表规则、时间戳、提交格式），
  人和 coding agent 都适用。
- [docs/guides/](guides/)：分场景的开发指引（后端、前端、HeroUI、样式、i18n、Docker 执行端、
  自带机器执行端、环境排查）。
- `server/` 下每个包都有自己的 README。

## 目录结构

```
server/           后端 Go（module github.com/chyroc/ama/server）
  cmd/amad/       服务进程入口
  cmd/ama-cli/    ama 命令：容器里给 agent 用，也是自带机器的 daemon
  auth/ authz/    登录（本地账号 / OIDC / GitHub / API Key）与按空间角色的授权
  core/ feishu/   业务：空间、成员、agent、技能、方舟凭证、运行历史；飞书机器人
  shared/infra/   接入层：pg、redis、s3、ma（方舟）、docker / dockertools、shelltools、lark、oidc …
  repository/schema/  全部 DDL，一表一文件
web/              前端：Vite + React 19 + TypeScript + Tailwind 4 + HeroUI 3
cli/              ama 命令的 npm 包装
sandbox-image/    会话容器镜像
deploy/           镜像内默认配置
docs/             设计文档与开发指引
scripts/          图标生成、种子导出、方舟 agent 初始化等脚本
```
