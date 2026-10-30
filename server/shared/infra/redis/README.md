# shared/infra/redis

Redis 接入层。**只做一件事：分布式选主**——飞书长连接，以及定时任务调度器、
worker poll 这类「多实例里只该有一个在跑」的后台循环。

不放缓存、不放队列、不存业务数据——那些都在 PostgreSQL 里（`shared/infra/pg`）。

## 为什么这个仓库需要 Redis

飞书**单个应用只允许一条长连接**。

多个 amad 实例各自裸连同一个飞书应用时，飞书会把先来的踢掉，实例之间互相驱逐。
麻烦的是这个失败**不报错**：表现为入站消息随机丢失——这一条进了 A 实例，下一条进了 B，
被踢掉的那段谁都没收到。用户看到的是「机器人有时理我有时不理」。

所以建连前必须先抢一把锁，且**抢不到就绝不建连**（fail-closed）。宁可这个实例不收
消息，也不能两个实例抢同一个应用。

租约键形如 `lark:long_connection:leader:<appID>`。

## 为什么不用 PG 的 advisory lock 省掉一个依赖

认真考虑过。advisory lock 绑在会话上，连接断了锁自动释放——听起来正合适。

但那个「自动」取决于 PG 何时发现连接已死，而连接池还会把同一个会话复用给别的查询，
于是租约到底什么时候过期变得不可预测。选主要的恰恰是**可预测的过期时间**：知道最坏
情况下有多久的空档，才能决定续约间隔。Redis 的 `SET NX PX` 是显式的毫秒级 TTL，
读代码就能算出来。

## 租约语义

| 操作 | 实现 | 说明 |
| --- | --- | --- |
| `Acquire` | `SET key token NX PX ttl` | key 已存在就失败，返回 `(nil, false, nil)` |
| `Renew` | Lua：token 匹配才 `PEXPIRE` | 返回 false 表示锁已不是自己的 |
| `Release` | Lua：token 匹配才 `DEL` | 不是自己的就什么都不做 |

### 三个坑，前两个已经用测试锁住

**一、比对 token 不能省。** A 的租约过期后 B 抢到了锁，这时 A 迟到的续约会把 B 的锁
续成 A 的、迟到的释放会直接把 B 的锁删掉——两个实例就同时建连了，正好是要防的事。
所以每个 `Lease` 带一个随机 token，续约和释放都先比对。
见 `TestStaleHolderCannotTouchNewLease`。

**二、比对 + 操作必须原子。** 不能写成「先 GET 比对、再 PEXPIRE」两步：两步之间租约
可能过期、锁可能被别人抢走。所以用 Lua——Redis 里的脚本是原子执行的。

**三、`Release` 要传不会被取消的 ctx（`context.WithoutCancel(ctx)`）。** 释放发生在
关停路径上，那时外层 ctx 往往已经取消，拿它去释放会直接失败，锁释放不掉，
下一个实例白等一个 TTL。

## 用法

```go
// 本地：docker-compose 起的 Redis，直连
client, err := redis.New(ctx, redis.Config{Addr: "127.0.0.1:16379"})
if err != nil {
    return err
}
defer client.Close()

lease, ok, err := client.Acquire(ctx, "lark:longconn:cli_xxx", 20*time.Second)
if err != nil || !ok {
    return err // 抢不到就不建连
}
defer lease.Release(context.WithoutCancel(ctx))

// 起一个 goroutine 每 5 秒续一次；Renew 返回 false 要立刻停掉长连接
```

`New` 会 **ping 一次**再返回。go-redis 的 `NewClient` 只是建了个池不会真连，地址写错、
Redis 没起都要等第一次真实操作才暴露，那时报错离真实原因已经很远了。

## 连接方式

底层是开源的 `github.com/redis/go-redis/v9`，直连 `host:port`，可带密码与库序号。
没有集群、服务发现那一套。

ctx 会一路传给 go-redis，所以调用方的超时与取消对单次命令生效；`dialTimeout` /
`opTimeout` 是没传超时时的兜底。

Lua 脚本的返回值按 type switch 解读（`int64` 或 `string` 的 `1` 都算成功），
不用 `.Int64()`：形状一变就成了「err != nil，续约失败」，而续约失败在选主里意味着
**主动让位**——表现是「莫名其妙频繁切主」，报错却只说类型转换。

## 配置

`server/config.yaml` 的 `redis` 段（模板 `config.example.yaml`），字段只有
`addr` / `password` / `db` / `dialTimeout` / `opTimeout`。环境变量
`AMA_REDIS_ADDR` / `AMA_REDIS_PASSWORD` 优先级更高。

**整段可以留空**：那时飞书长连接不启用，服务照常起，只是收不到飞书消息；调度器这类
后台循环退回「每个实例都跑，靠 PG 的原子认领保证不重复」。启动日志会明说这两件事。

⚠️ 「留空」的判定是 **`addr` 为空串**（`config.Redis.Enabled()`）。所以上线的配置里
别带着本地地址——**带着本地地址上线会启动即退出**，而日志说的是「连 Redis 失败」，
完全不指向「配置里留了一行本地地址」。

## 本地怎么起

```bash
docker compose up -d          # 在仓库根，PG 与 Redis 一起
docker compose ps             # 等两个都 healthy
```

端口是 **16379** 不是 6379（与 PG 用 15432 同理：本机常有别的 Redis 占着默认端口，
撞上了报错不指向端口冲突）。容器显式关掉了 RDB 快照——选主的锁不需要持久化，租约本来
就有 TTL，落盘反而会让重启后残留一把没人持有的锁，白等它过期。

## 测试

`go test ./shared/infra/redis/` **要一个真 Redis**，连不上就 `t.Skip` 跳过而不是失败——
所以没起容器的机器上 `go test -race ./...` 仍然全绿，符合仓库「全部离线」的约定。

不用假实现是有意的：这个包总共就 `SET NX PX`、两段 Lua、一个随机 token，前两件的语义
完全由 Redis 定义。用假实现测等于测我自己写的假 Redis 对不对，而真正要防的坑
（Lua 的原子性、过期后被抢走时续期必须失败）恰恰是假实现最容易「测通」的地方。

配置的默认值与校验（空白当没配、回落默认地址）有纯逻辑用例，不需要真 Redis。
