# Docker 执行端：部署、配置与安全

agent 的六个工具默认在 Docker 容器里执行：**每个方舟会话一个容器 + 一个工作区 volume**。
设计细节（状态机、多实例、回收）见
[server/shared/infra/dockertools/README.md](../../server/shared/infra/dockertools/README.md)，
这里只讲「怎么把它跑起来、跑在哪、有什么风险」。

## 最小可用

```bash
# 1. 构建会话容器镜像（在仓库根；上下文要含 server/，镜像里会编一份 ama CLI）
docker build -t ama-sandbox:latest -f sandbox-image/Dockerfile .

# 2. amad 能访问 Docker daemon 即可：默认连 unix:///var/run/docker.sock，
#    或者用 DOCKER_HOST / docker.host 指到别处（只支持 unix:// 与 tcp://，不支持 TLS）
./dev.sh
```

启动日志里应当有一行 `Docker 执行端已连上 daemon`，带着 daemon id、镜像、工作目录与空闲策略。
看到的是 `Docker 执行端装配失败` 就别往下查对话了：agent 一调工具就会报错。

整套用 compose 跑：`docker compose up -d --build`（PostgreSQL + Redis + 会话镜像 + amad）。

## 配置

全部在配置文件的 `docker` 段，或对应的环境变量（环境变量优先）。模板见
[server/config.example.yaml](../../server/config.example.yaml)。

| 键 | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| `docker.enabled` | `AMA_DOCKER_ENABLED` | `true` | 只用宿主机执行端时关掉 |
| `docker.host` | `AMA_DOCKER_HOST` | `DOCKER_HOST` → `unix:///var/run/docker.sock` | daemon 地址 |
| `docker.image` | `AMA_DOCKER_IMAGE` | `ama-sandbox:latest` | 要有 bash、coreutils（含 timeout）、GNU find、grep |
| `docker.workdir` | `AMA_DOCKER_WORKDIR` | `/workspace` | 工作区 volume 挂载点，技能装在 `<workdir>/skills` |
| `docker.prefix` | `AMA_DOCKER_PREFIX` | `ama` | 容器 / volume / label 前缀 |
| `docker.cpus` | `AMA_DOCKER_CPUS` | 不限 | 每个容器的核数（可小数） |
| `docker.memory` | `AMA_DOCKER_MEMORY` | 不限 | 如 `2g`；写错启动失败 |
| `docker.pidsLimit` | `AMA_DOCKER_PIDS_LIMIT` | 不限 | 进程数上限 |
| `docker.network` | `AMA_DOCKER_NETWORK` | daemon 默认（bridge） | `none` 彻底断网 |
| `docker.idleAction` | `AMA_DOCKER_IDLE_ACTION` | `pause` | `pause` / `stop` / `none` |
| `docker.containerTTL` | `AMA_DOCKER_CONTAINER_TTL` | `24h` | 闲置多久删容器（volume 保留），`0s` 不删 |
| `docker.workspaceTTL` | `AMA_DOCKER_WORKSPACE_TTL` | `168h` | 闲置多久删工作区 volume，`0s` 不删 |
| `worker.concurrency` | `AMA_WORKER_CONCURRENCY` | `20` | 每个方舟 environment 同时处理几条 work |

### 多实例

- **所有实例连同一个 daemon**：工作区跨实例共享，会话落到哪个实例都接得上。推荐。
- **各连各的 daemon**：能跑，但会话换了实例就换了 daemon，对用户来说工作区被重置了；
  页面上看别的 daemon 上的容器会报「容器在另一个 Docker daemon 上」。
- 同一套 AMA 的所有实例 **`prefix` 必须一样**；同一个 daemon 上跑两套互不相干的 AMA 时
  **必须不一样**（回收巡检按 label 认容器）。
- 配了 Redis 时回收巡检按 daemon 选主，没配时每个实例都扫（结果正确，只是重复）。

### 容器里的 `ama` 命令要能打回 amad

勾了「AMA」授权的 agent 在容器里跑 `ama`，它打的是注入的 `AMA_BASE_URL`（= `server.publicURL`）。
会话容器在 daemon 的网络里，**`http://localhost:9999` 在容器里指的是容器自己**。
要么让 `publicURL` 是一个容器里也解析得到的地址，要么用 `docker.network` 把会话容器接进
与 amad 同一个网络（compose 里是 `ama-net`），再把 `publicURL` 配成 `http://amad:9999`
（那时浏览器访问另做反代）。

## ⚠️ 安全：docker.sock 等于宿主机 root

**能访问 Docker daemon 的进程，等于拥有那台机器的 root。** 它可以起一个
`--privileged -v /:/host` 的容器，然后读写宿主机任意文件。所以：

1. **amad 本身必须当成高权限服务部署**：别把它暴露给不可信的人，尤其是固定 API Key、
   `_debug` 开发路由（`server.devRoutes`）。
2. **会话容器拿不到 socket**——amad 不会把它挂给会话容器，agent 的命令碰不到 daemon。
   但会话容器与宿主机**共享内核**，容器隔离不是虚拟机隔离：内核漏洞就是逃逸面。
   会话容器默认开了 `no-new-privileges`，没有 `--privileged`，也不挂任何宿主机目录。
3. 要更强的隔离，任选其一或叠加：
   - 把 `docker.host` 指向**一台专门跑会话容器的机器**（与 amad、数据库分开）；
   - daemon 开 `userns-remap` 或用 rootless Docker（容器里的 root 映射成宿主机普通用户）；
   - 用 gVisor（`runsc`）/ Kata 之类的运行时当 daemon 默认运行时；
   - `docker.network: none` 断网，或接一个只放行必要出口的网络；
   - `docker.cpus` / `docker.memory` / `docker.pidsLimit` 限住单个会话能吃多少资源。
4. agent 能读到注入进容器的凭据（`env` 一句就够），工具输出出口会把它们抹掉，但这挡不住
   刻意外泄。给 agent 的授权按「它会被看见」来给。

## 网页终端

「打开终端」是在会话容器里 `docker exec -it bash -l`。连接握在开终端的那个实例上，
跨实例的按键经 PG 队列转发过去（见 `server/core/sandboxterm.go`）。终端里**没有**注入的
会话 env（那些只跟着工具调用下发）。

## 排查

```bash
docker ps -a --filter label=ama.managed=true          # 本服务建的全部会话容器
docker volume ls --filter label=ama.managed=true      # 全部工作区
docker logs <容器>                                     # 保活进程没有日志，看 amad 的日志
```

amad 日志里按 `ark_session` 搜，容器的建 / 重建 / 暂停 / 停止 / 回收都有一行。
