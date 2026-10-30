# 环境坑与凭据

**读它的时机**：装依赖、跑构建、找凭据，或任何「本机跑不起来」的排查。

这些坑的共同点是**报错不指向真实原因**——照着报错查会绕远路。

## 本地凭据

**放仓库根的 `.env`**（已 gitignore，含 `ARK_API_KEY` 等变量，模板是 `.env.example`）。
跑 example 前：

```bash
set -a; source .env; set +a
```

## 本地依赖：`docker compose up -d postgres redis s3`

```bash
docker compose up -d postgres redis s3   # 仓库根，起 postgres:17-alpine、redis:7-alpine、rustfs/rustfs:1.0.4
docker compose ps                        # 等 STATUS 变 healthy 再连
cd server && cp config.example.yaml config.yaml
```

**对象存储（`s3`）不是可选的。** 文件字节（附件、技能文件、个人文件）只存对象存储，
PG 档下没配 `s3.bucket` amad 直接拒绝启动，报错里说的就是这件事。本地 RustFS 在宿主机
19000 端口，`config.example.yaml` 里的 `s3` 段已经对好。

⚠️ **老的本地库要 `down -v` 重建一次**（或手动 `alter table issue_attachments drop column content`
等）：附件表删掉了 `content bytea` 列、`file_id` 改成 not null，`blob_objects` 表也删了。
DDL 是 `create table if not exists`，**不会改已经存在的表**——老库上传附件会报
`null value in column "content"`，报错不指向「表结构是旧的」。

⚠️ **会话容器要接在 `ama-net` 上才拉得到技能包**：`config.example.yaml` 里
`docker.network: ama-net`、`s3.sandboxEndpoint: http://ama-s3:9000` 是一对。
`docker compose up -d s3` 会建出 ama-net；容器拉不到技能包时不报错，只是技能里只有
SKILL.md、附件全缺（amad 日志里一条「技能包下载失败」Warn）。

连接配置在 `server/config.yaml`（gitignore，模板是 `config.example.yaml`）。

**改了 `POSTGRES_INITDB_ARGS` 必须 `docker compose down -v`。** 编码和 locale 只在
initdb（第一次建数据卷）时生效；数据卷已存在时 `up -d` 根本不会再跑 initdb，
于是你以为改了其实没改。本地刻意用 **UTF8 / Collate C / CType C**，生产库建议同样设置。

**端口是 15432 不是 5432**，避开宿主机上可能已占用的 5432——撞上了报错不指向端口冲突。

**PG 与 Redis 只绑 `127.0.0.1`，口令来自 `.env`**（`./scripts/init-env.sh` 随机生成，compose 缺了就拒绝启动）。
`dev.sh` 会 `source .env`，所以本地进程拿得到同一份口令；直接 `psql` / `redis-cli` 时要自己从 `.env` 取。
PG 口令只在第一次建数据卷时生效，数据卷已存在时改它要 `ALTER USER` 或 `down -v`。

**连托管 PostgreSQL 时 IPv6 不要加方括号。** 不少云厂商文档说「IPv6 要用方括号包裹」，
那只适用于 URI 形式的 DSN；我们用的 libpq key=value 形式里方括号会变成主机名的一部分，报
`lookup [fdbd:...]: no such host`。详见 `server/shared/infra/pg/README.md`。

## 前端：`npx vite` 直接跑会 EMFILE 挂掉

有些机器上 `fs.inotify.max_user_instances` 只有 128 且已被其它进程占满（**不是 fd 上限问题**，
fd 软限已在 `web/scripts/dev.sh` 里提到 65535）。`vite.config.ts` 因此配了
`server.watch.usePolling`。

**单独启前端一律在 `web/` 下用 `./scripts/dev.sh`**，别直接 `npx vite`。


## e2e 用系统 Chrome

配置里走 `channel: "chrome"` 指向
`/usr/bin/google-chrome`，**不需要 `playwright install`**。

## 其它接入层的环境问题

方舟凭据这几条跟具体接入层绑得紧，放在 [infra.md](infra.md) 里。
agent 一调工具就报错、启动日志有 `Docker 执行端装配失败`：amad 连不上 Docker daemon
（`/var/run/docker.sock` 或 `DOCKER_HOST`），或会话镜像没构建，见
[docker-executor.md](docker-executor.md)。
