# pg —— PostgreSQL 接入层

把 PostgreSQL 包成一个 `database/sql` 连接池 + 一组错误判别函数。

目标库是**标准 PostgreSQL**：生产可以是自建或云厂商托管的 PostgreSQL，
本地开发是 `docker compose` 起的 `postgres:17-alpine`。两者都用 `host:port` + 用户名密码、
走 `pgx/v5/stdlib`，**同一条代码路径**——本地跑通的就是生产要跑的，中间没有第二条未测过的分支。

| 文件 | 职责 |
| --- | --- |
| `standard.go` | 怎么连上：`StdConfig`、DSN 拼装、`NewStandard` |
| `client.go` | 共享的 `Client`（Raw / Database / Ping / Stats / Close） |
| `query.go` | Exec / Query / QueryRow / WithTx |
| `errors.go` | SQLSTATE 常量与判别函数 |

## 用法

```go
client, err := pg.NewStandard(ctx, pg.StdConfig{
    Host:     "127.0.0.1",   // IPv6 地址**不加方括号**
    Port:     15432,
    Database: "ama",
    User:     "ama",
    Password: os.Getenv("AMA_PG_PASSWORD"),
})
if err != nil {
    return err
}
defer client.Close()

// 事务：返回 nil 提交，返回 error 回滚，panic 也回滚
err = client.WithTx(ctx, func(tx *sql.Tx) error {
    if _, err := tx.ExecContext(ctx, "insert into a(id) values($1)", 1); err != nil {
        return err
    }
    _, err := tx.ExecContext(ctx, "update b set n = n + 1 where id = $1", 1)
    return err
})

// 错误判别
if pg.IsUniqueViolation(err) { /* 这条已经存在 */ }
if pg.IsRetryable(err)       { /* 退避重试 */ }
```

实际用法是从 YAML 加载配置（[`shared/infra/config`](../../shared/infra/config/config.go) +
`server/config.example.yaml`），然后 `cfg.PG.ToStdConfig()`。

## 验证连通性

```bash
docker compose up -d postgres         # 仓库根，起 PG 17
docker compose ps                     # 等 healthy
cd server && cp config.example.yaml config.yaml
go run ./cmd/amad                     # 启动日志里有「已连上 PostgreSQL」即通
```

## 坑一：IPv6 在 key=value DSN 里不能加方括号

不少文档说「连接字符串中的 IPv6 地址必须使用方括号包裹」——**那条只适用于 URI 形式的 DSN**
（`postgres://user:pass@[::1]:5432/db`）。本包拼的是 libpq 的 key=value 形式，
那里的方括号会被当成主机名的一部分。实测 pgx v5.8.0：

```
host=[::1]  →  hostname resolving error: lookup [::1]: no such host
host=::1    →  dial tcp [::1]:5432: connect: connection refused   ← 正常到达网络栈
```

照文档抄反而错。而且 `pgx.ParseConfig` 对两种写法都不报错（URI 形式会正确剥掉方括号，
key=value 形式原样保留），**解析阶段发现不了，要等连接时才炸**。

`toStdDSN` 会主动把首尾方括号剥掉（填了也能用），`TestToStdDSNIPv6NoBrackets` 守着这条。

## 坑二：DSN 键名写错不报错，会静默失效

pgx 不认识的键会被当成 PostgreSQL 的**启动参数**原样下发，于是 `sslmode` 实际没生效而你
以为设了。`ssl_mode` / `db_name` / `database` / `username` 这类易错写法在
`misspelledKeys` 里被显式拦下来，报错会指出正确写法。

`TestStdDSNOnlyHasLibpqKeys` 反过来也守一遍：DSN 里出现的键必须都在白名单内。

## 坑三：密码不能进日志

DSN 原文含明文密码，而「连不上」时最想打的就是连接参数。用 `StdConfig.Redacted()`，
密码渲染成 `***`。`NewStandard` 的错误信息用的就是它，`TestRedactedHidesPassword` 和
`TestNewStandardErrorHasNoPassword` 守着。

## 边界：库和账号要事先建好

**本包不建数据库、不建角色。** 不需要超级用户（托管 PG 通常也不给），但账号要能在库里
建表，最省事的做法是建库时把它设成库的 Owner（`CREATE DATABASE ama OWNER ama;`）。
所以本包的起点是「一个已经存在的库 + 一个已经有权限的账号」。

建议建库时选 **utf8 / Collate `C` / CType `C`**，`docker-compose.yml` 用
`POSTGRES_INITDB_ARGS` 设的就是这三项。选 `C` 而不是 `zh_CN.utf8` 是有意的：
`C` 按字节比较，排序和 `LIKE 'prefix%'` 都能直接走 B-tree 索引；要中文语义排序时
按列或按查询加 `COLLATE "zh_CN.utf8"` 即可。反过来库级 collation 建完就改不了。

## 连不上时：超时与 connection refused 是两回事

- **`connection refused`**：包到了主机，端口上没人听——查端口、查服务有没有起来。
- **超时**（尤其是 `ping` 通而 TCP 全部超时）：包被中途静默丢弃，通常是防火墙 / 白名单
  只放行了 ICMP。不是路由不通（否则 ping 不会回），也不是凭据问题（还没到认证那一步）。

云厂商托管实例的内网域名往往是 VPC 内部的私有 DNS，VPC 外解析不出来（NXDOMAIN）——
那种情况配置里填 IP。

## 设计取舍

沿用 [`shared/infra/ma`](../ma/README.md) 的约定：

**薄封装。** `Exec` / `Query` / `QueryRow` 是 `*sql.DB` 同名方法的转发，只做「空 SQL
本地拦掉」和「给 error 补上是哪条语句失败」。返回值一律是标准库的 `*sql.Rows` /
`sql.Result`，不做二次 DTO 映射。`Raw()` 把 `*sql.DB` 直接交出去，用于 `Stats`、
`Conn`、`Prepare` 这些本包没包的能力。

**只给回调式事务，不暴露裸 `Begin`。** 手写事务有两个几乎必踩的坑：忘了在错误分支
`Rollback`（连接被占住直到 ctx 超时），以及 `fn` 里 panic 之后连接永远回不到池子。
池子是有限的，漏几个就整个服务卡死。所以统一走 `WithTx`，panic 会先回滚再原样抛出
（不吞，堆栈不丢）。回滚本身失败时两个错误都留着——只报「rollback failed」会把真正的
业务错误盖掉。

**错误判别只看 SQLSTATE，不匹配文本。** 报错文本随 PostgreSQL 版本和 locale 变，
SQLSTATE 是标准里钉死的。

**UPDATE / DELETE 一律带 WHERE。** 有些托管 PG 支持 `safeupdate` 插件，一旦被加载，
不带 WHERE 的 UPDATE / DELETE 会被直接拒掉。

## 池默认值的依据

| 项 | 默认值 | 依据 |
| --- | --- | --- |
| `MaxOpenConns` | 25 | PostgreSQL 默认 `max_connections` 是 100（本地容器就是 100），托管实例按规格通常更多。25 给多实例和运维工具留了余量 |
| `MaxIdleConns` | 5 | 够复用又不长期占着服务端的 backend 进程 |
| `ConnMaxIdleTime` | 5 分钟 | 同上 |
| `ConnMaxLifetime` | 30 分钟 | 让连接定期重建。主备切换后旧连接指向已降级的节点，定期重建能让池子自己收敛过去 |

`TestStdPoolDefaultsAreSane` 断言默认值不超过 50——多个服务实例共享一个库时，
默认值太大容易一起把 `max_connections` 顶满。

## 测试

```bash
go test -race ./shared/infra/pg/    # 32 个用例，全部离线，无需数据库和凭据
```

覆盖「发请求之前」的逻辑：配置校验、DSN 拼装（含易错键拦截、libpq 引号规则、IPv6
方括号、拼装结果稳定性）、池参数、错误判别（含把 `PgError` 从两层 `%w` 包装里挖回来）。
事务的三条控制流路径（提交 / 回滚 / panic）用一个最小的假驱动跑，不引第三方 mock 依赖。

真实连通性不在单测里，见上面「验证连通性」。
