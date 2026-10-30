# orchestrator —— 编排层，handler 的唯一入口

`httpd` / handler 只认这里。跨域协作也在这里——因为**域包之间零 import**，
需要两个域一起完成的事只能往上走一层。

> **现在的状态（步骤 17 之后）**：本包直接持有**九个域包**的 Service，
> 依赖方向已经扳过来了——判据是 `go list -deps ./orchestrator/... | grep -c service/`
> 从 0 变成 9（11b 给 skill 长出了 Service）。
>
> ✅ **十个域各自成包了**（步骤 17 切了九个；agent 与 skill 后来从
> workspace 拆出，skill 又把 market 并了进来）：`orchestrator/<域>/service.go`，
> 接口就叫 `Service`（`issue.Service` / `agent.Service` / `skill.Service`），
> 域名由包名承担。
>
> ✅ **handler 已经只认本包了**：它搬出了 core（现在是 `server/apihandler`），
> 按域持有十个窄接口字段（步骤 16 删掉了那个九域并集 `orchestrator.Service`；
> agent 拆出、skill 并入 market 后字段数成十），
> 不再是 `*core.Service` 那 254 个。判据两条，都机器可判：
>
> ```
> go list -deps ./apihandler | grep -c 'ama/server/core$'   # 0
> ls server/core/handler*.go                                # 无
> ```
>
> 于是 `core/handlerservice.go` 那份**手抄的接口副本**整个删掉了——
> 「handler 能调什么」现在只有一个定义处，就是 `orchestrator/<域>/service.go`
> 这十份清单。
>
> 仍是**混合状态**，且这是预期结果：那 133 个方法里 38 个还只有
> `*core.Service` 能满足（chat / worker / sandbox / 偏好 / 反馈 /
> 用量），它们走 `Config.Fallback` 那个接口回落。
> 十二个 service 域包里现在九个有 handler 会调的 `*Service`（skill 在 11b
> 长出了自己的）；剩下三个（arkrun / arkbackoff / integration）搬出去的是
> 类型与 provider，不在这 133 个里。细节见 `orchestrator.go` 的文件头。
>
> **别造占位 `.go`** ——那种文件会被下一个人当成真代码。

## 依赖方向

```
httpd → apihandler → orchestrator          装配与 Routes/Mux（顶层三个文件）
                   → orchestrator/<域>     十份窄接口清单，一域一包
                        ↓
                     service/<域> → domain / storemodel
```

顶层 `orchestrator` 是唯一允许"看见多个域"的地方（它 import 那十个子包做装配）。
往下单向，不许有人反向 import 编排层。

⚠️ **十个子包之间零 import**，一个都没有。判据要用 `go list` 而不是 grep
（源码注释里满是 `orchestrator/<域>` 这种路径字样，grep 全是假阳性）：

```bash
# 每个子包依赖的兄弟包数，十行都必须是 0
for d in orchestrator/*/; do n=$(basename "$d"); \
  printf '%-11s%s\n' "$n" "$(go list -deps ./$d | grep 'orchestrator/' | grep -vc "orchestrator/$n$")"; done
```

它们各自只依赖 `domain` / `storemodel`（workspace / agent 那两个另加
`service/*`——前者三个、后者一个——因为组合体在它们里面）。要引用
兄弟域就得加一行 import，那一行 code review 看得见——这正是切包换来的东西。

## ⚠️ 不写 161 个空壳转发，也不用代码生成

**做法：编排层定义「消费方视角的窄接口」，一个域一个包**
（`orchestrator/<域>/service.go`）。接口**只需要签名、不需要函数体**，省掉的正是那些
空壳。域包的具体类型隐式满足它，零改动。

```go
// orchestrator/agent/service.go —— 这就是它的实际形状（曾是单方法示例，
// agent 成包之后示例变成了实物，照实列全 9 个）
package agent

type Service interface {
	CreateAgent(ctx context.Context, workspaceID string, draft domain.AgentDraft) (domain.Agent, error)
	DeleteAgent(ctx context.Context, id string) error
	GetAgent(ctx context.Context, id string) (domain.Agent, error)
	GetAgentArkConsole(ctx context.Context, agentID string) (domain.ArkConsoleRef, error)
	LatestAgentSyncTask(ctx context.Context, agentID string) (domain.AgentSyncTask, error)
	ListAgents(ctx context.Context, workspaceID string) ([]domain.Agent, error)
	RetryAgentSync(ctx context.Context, agentID string) (domain.AgentSyncTask, error)
	SetAgentIntegrations(ctx context.Context, id string, ids []string, creds domain.AgentCredentialIDs) (domain.Agent, error)
	UpdateAgent(ctx context.Context, id string, draft domain.AgentDraft) (domain.Agent, error)
	// 域包新增的方法默认不在这儿 —— 要暴露给 handler 就显式加一行
}
```

⚠️ **接口名一律是 `Service`，不带域名前缀**（`issue.Service`，不是
`issue.IssueService`）——包名已经把域说清楚了，前缀是重复。
⚠️ **签名里一律写 `domain.X`，不写 `core.X`**：本包（含十个子包）不许 import core，
判据 `go list -deps ./orchestrator/... | grep -c 'ama/server/core$'` 必须是 0。

**这才是「域包不对 handler 暴露」的实现方式**：域里新增一个方法，默认 handler 看不见；
要开放就得来这个文件加一行。代价是每开放一个方法多改 1 行，但改在自己域的文件里，
不制造冲突面。

⚠️ **嵌入 `*agentsvc.Service` 不满足这个约束**——那样 handler 会拿到域的全部方法，
「哪些对外」就退化成一句无人执行的约定，没有任何编译期抓手。

## 两个跨域 switch 改成注册式

`WorkspaceOf`（覆盖 7 种资源）与回滚（覆盖 3 个域）不属于任何单一域。

**保持 switch 的话，每搬一个域都要回来改它一次**——那正好精确复制了 `core/store.go`
今天的冲突面（近 30 天被改 51 次），而拆包的目的之一就是消掉这种"每次都要动同一个
文件"的结构。

改成 `map[ResourceKind]func(...)`，**各域填自己那格**。搬 issue（步骤 2）时顺手把
机制建起来。

## 什么该进编排层，什么不该

| | 归哪 |
| --- | --- |
| 要同时读/写两个以上域 | **这里** |
| 一个域内部的规则、只碰自己的 Store | `service/<域>` |
| 只看类型自己字段的判断与归一 | `model/apimodel`，跟类型同文件 |
| HTTP 解析、状态码映射、写响应 | 留 handler |

⚠️ **别把编排层写成第二个 core。** 它的正常形态是薄的：取数据、按顺序调几个域、
把结果拼起来。一旦某个编排函数里开始出现只跟单个域有关的判断，那段就该沉回
`service/<域>`——否则拆完之后所有逻辑只是从 `core` 平移到了 `orchestrator`，
换了个包名而已。

## handler 迁移时注意

- **路由 pattern 逐字不变**，则 `authz/matrix.go` 无需改。判据是
  `go run ./cmd/gen-openapi` 后 `openapi.json` **零 diff**（它从 matrix 生成，
  没漂移即证明路由没动过）。
- ⚠️ **新增 HTTP 路由必须在 `authz/matrix.go` 里声明权限，否则启动 panic**。
  本层不改路由，所以这条在搬家期间用不上——但真加路由时别忘了。
- **禁止顺手改行为、改命名、补功能、修 bug**（发现 bug 另开 MR）。

## ⚠️ 装配代码（持有 service 层的域包）在十个域包自己手里

**每个 `orchestrator/<域>/` 都有一个 composite.go**：它持有 service 层的域包、
提供 `New(...)` 把它装成本域的 Service——运行时执行的就是 service/<域> 的实现
（方法靠嵌入提升，零手写转发）。将来真正跨域的编排方法也写在各域的 composite
上，那才是编排层该写代码的地方；单域的业务规则去 `service/<域>`。

两种形状，按来源数分：

- **单来源（八个域）**：issue / suggestion / revision / ark / skill 整个由域包
  满足，composite 只嵌一个 `*xxxsvc.Service`；
- **跨来源（两个域）**：`workspace.Service`（47 个方法：14 个来自
  `service/workspace`、11 个转发到 schedule/run、22 个回落 core）与
  `agent.Service`（9 个方法：7 个来自 `service/agent`、2 个回落 core），
  composite 嵌「域包 + Fallback」两份；
- **还在 core 的 sandbox 域**：顶层直接填 `cfg.Fallback`，
  没有 service 层可持有——等它搬出 core 后同样长出 composite。

**别为了让顶层能 new 它而把结构体导出**——「谁嵌谁、哪个字段匿名」是纯装配细节，
导出等于把它摊给包外。理由与那个「同深度同名即歧义」的坑写在
`orchestrator/workspace/composite.go` 的文件头（单来源那八份与 agent 那份：
嵌入名不撞车，零转发）。
