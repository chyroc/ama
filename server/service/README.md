# service —— 每个资源实体一个子包

一个域一个子包：`service/issue/`、`service/agent/`、`service/run/`……
每个子包里是**该域的 Service + 它自己的 Store 接口 + 该域的逻辑**。

> **本目录现在只有这份 README，一个子包都还没建。** 第 0b 步刻意只立骨架、不搬代码
> （这样几十个并发 worktree 能立刻开工不撞车）。**别造占位 `.go` 让 `go build`
> 好看**——那种文件会被下一个人当成真代码。
>
> 搬家顺序与每步验证在拆包计划里，第一个搬的是 issue（最便宜：`issueStore` 已分离、
> 路由已独立、只 5 个文件、有 e2e 覆盖、且不调任何其它域的 Service 方法）。

## 依赖方向

```
httpd → orchestrator/<域> → service/<域> → model/apimodel
                                        ↘ model/persistmodel → model/apimodel
```

**两条硬约束：**

1. **域包之间零 import。** `service/agent` 不许 import `service/skill`，反过来也不行。
2. **跨域协作一律往上走 `orchestrator`。** 需要别的域的数据，就让编排层取好了传进来。

为什么不许横向 import：域之间的引用一旦开个口子就会长成网，而网是有环的
（agent 要 skill、skill 要 workspace、workspace 判权要 member……）。Go 会在成环那天
才报错，那时已经缠了几十处，只能靠"再抽一个 common 包"解，于是又回到今天这个局面。
**往上走虽然多一层，但方向是单向的，永远不会成环。**

## 每个域自己声明 Store 接口

`service/issue/` 里写 `type Store interface { ... }`，**只列这个域用得着的方法**。

不要复用一个全站大 Store。理由是隐式接口：PG 与内存两个实现**不需要任何改动**就自动
满足这些窄接口（方法都在），而窄接口让「这个域碰了哪些表」在编译期看得见。

## ⚠️ 领域规则不放这里，跟着它的类型走

`ValidIssueStatus` 放 `model/apimodel/issue.go`，**不要**放 `service/issue/`。

这条容易反着做——那些函数看起来就是"业务逻辑"。但 `model/apimodel` 现有
**74 个类型 + 25 个导出函数**，那 25 个函数（`ValidIssueStatus`、
`NormalizeFollowUpMode`、`IsValidEnvKey`、`ValidSkillName`…）全是"只看类型自己字段"
的判断与归一。**把它们搬进 service 正是这次重构要修的病**，别在新结构里重犯。

判据：**这个函数只看这个类型自己的字段吗？**

| | 归哪 |
| --- | --- |
| 只看自己的字段（枚举合法性、归一化、格式校验） | `model/apimodel`，跟类型同文件 |
| 要读库、要调别的域、要发事件、要碰时钟或外部系统 | **这里** |

放错的代价不是"不好看"：规则留在 service，别的域想复用就得 import 那个域的 service
——而域包之间零 import，于是只能抄一份，两份慢慢判得不一样，且没有任何东西会报错。

## 域包的方法默认对 handler 不可见

handler 只认 `orchestrator`，而编排层用的是**它自己定义的窄接口**（消费方视角），
不是嵌入 `*issue.Service`。所以这里新增一个方法，**默认 handler 看不见**，
要暴露得去编排层显式加一行。

代价是每开放一个方法多改 1 行，收益是「域包不对 handler 暴露」有编译期抓手，
而不是靠约定。细节见 [orchestrator/README.md](../orchestrator/README.md)。

## 搬家时的注意事项

- **禁止顺手改行为、改命名、补功能、修 bug。** 发现 bug 另开 MR——混在搬家 diff 里
  没人 review 得出来。搬家 MR 的价值全在「diff 里只有位移」这一点上。
- **测试与生产代码同量**：现 core 的生产代码是 84 文件 / 37183 行，测试是
  68 文件 / 35726 行，而且测试大量使用未导出符号（搬包名就得跟着动）。
  **估工作量必须把测试算进去，否则低估一倍。**
- **MR 当天合掉不过夜**：`core/store.go` 近 30 天被改 51 次、`handler.go` 47 次
  （整个 `core/` 是 225 次），平均每天 1.7 次，隔夜就要 rebase。
- ⚠️ **碰 Store 的步骤必须 `docker compose up -d` 起 PG 再跑测试**——碰库的包连不上
  库时整组 `t.Skip`，**跳过时是绿的**。
- ⚠️ **路由 pattern 逐字不变**，则 `authz/matrix.go` 无需改。判据是
  `go run ./cmd/gen-openapi` 后 `openapi.json` 零 diff（它从 matrix 生成，
  没漂移即证明没动过路由）。
