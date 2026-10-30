# ama CLI

AMA 平台的命令行工具。操作空间、Agent、技能、运行历史、问题（issue）。

**主要使用场景是装进 AMA 的 Docker 会话容器**（`sandbox-image/Dockerfile` 已经编进去了），
让容器里的 agent 能操作用户自己的 AMA 数据。
也可以装在本机当普通 CLI 用。

## 安装

开源版的发包方式还没定，目前从源码编：

```bash
git clone https://github.com/chyroc/ama && cd ama/server
go build -o "$(go env GOPATH)/bin/ama" ./cmd/ama-cli
```

## 配置：三个环境变量

| 变量 | 必需 | 说明 |
| --- | --- | --- |
| `AMA_TOKEN` | ✅ | 凭据。容器里由平台自动注入 |
| `AMA_BASE_URL` | | 服务地址，默认 `http://localhost:9999`。容器里由平台注入 |
| `AMA_WORKSPACE_ID` | | 默认空间（形如 `ws_xxx`）。不给则命令要带 `--workspace` |

**凭据只从环境变量读，没有 `--token` 参数。** 这是刻意的：容器里的命令行会被完整
记录下来（进运行事件、落库、显示在浏览器里），凭据一旦进 argv 就等于永久泄漏。

## 用法

```bash
ama whoami                       # 我是谁、当前空间、能访问哪些空间
ama workspace list
ama agent list                   # 当前空间的 agent
ama skill list
ama skill upload <path>          # 上传技能包建技能（裸 SKILL.md 或 zip；--dry-run 先看解析结果）
ama run list                     # 运行历史（一次对话就是一条 run）
ama run events <runId>           # 对话消息在这里
ama issue list
ama issue create --title "..." --body "..."
ama issue update <id> --status closed
ama issue comment <id> --body "..."
```

没封装的接口用逃生口（平台一共 70 多个接口）：

```bash
ama api-doc                                   # 完整 OpenAPI 契约
ama api GET /api/workspaces
ama api POST /api/... '{"k":"v"}' --yes       # 写操作要 --yes
```

⚠️ **逃生口只发 JSON body**，所以收 `multipart/form-data` 的接口它打不到——
技能整包上传（`POST /api/workspaces/{ws}/skills/upload`）正是这一类，
它有自己的命令 `ama skill upload`。

`ama --help` 有完整清单。

## 支持的平台

`linux-amd64`（容器用的就是它）、`linux-arm64`、`darwin-amd64`、`darwin-arm64`。
`./build.sh` 一次编出四个。

## 开发

```bash
./build.sh    # 交叉编译四个平台，产物落 bin/
```

Go 源码在同仓库的 `server/cmd/ama-cli/`，说明见那里的 README。
