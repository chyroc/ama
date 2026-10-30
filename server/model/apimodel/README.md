# model/apimodel —— 给外部用的类型（进 HTTP 响应体）

handler 会 `writeJSON` 吐出去的东西：实体、请求体、响应体，以及**它们自己的领域规则**。

> **类型已经搬进来了（第 18 步）。** 原先住在 `internal/domain`，现在是本目录的
> 21 个 `.go` 文件，`internal/domain` 已删除。搬家是纯位移：`gofmt -r` 重写
> 5000 余处 `domain.X → apimodel.X`，**338 个 json tag 字面量集合逐字节比对为空**。
>
> `core` 里那 4 行 `type X = apimodel.X` 的别名桥保留着——它们是**同一个类型**
> （带 `=`），去掉会动到类型断言与 `errors.Is` 的语义，而收益只有「少一行」。

## 判据：会不会进 HTTP 响应

**不是「有没有 json tag」。** 这两条不等价，照后者筛会分错——
`persistmodel.ArkBinding` 有 9 个 tag 却一个都不进契约（那是 jsonb 列的格式）。

| 类型 | 归这里？ | 为什么 |
| --- | --- | --- |
| `Agent` / `Run` / `Skill` / `Issue` 等实体 | ✅ | handler 直接 `writeJSON` 出去 |
| `FeedbackPage` 这类响应体 | ✅ | 它**是**响应体（含 `[]FeedbackReport`） |
| `AgentDraft` 这类请求体 | ✅ | 前端按它构造 JSON，改名同样当场炸 |
| `ArkBinding`（9 个 tag） | ❌ 归 persistmodel | tag 是 `ark_bindings.doc` 这个 jsonb 列的格式 |
| `RunQuery` 等 5 个 `*Query`（0 个 tag） | ❌ 归 persistmodel | 只在 handler 里被构造成 Store 入参，没有 `writeJSON` 吐它们 |

判「是不是响应体」有个机械做法：搜这个类型有没有出现在 `writeJSON(` 的第三个实参
里（handler 已搬去 `apihandler/`，另有 `auth/` 与 `feishu/` 各一套）。

## 为什么必须跟 persistmodel 分开：两类的失败模式相反

| | apimodel | persistmodel |
| --- | --- | --- |
| 现有规模 | **338 个 json tag** | 14 个 |
| 改错了谁受影响 | 前端 | 线上存量数据 |
| 什么时候发现 | **当场**——页面白屏、字段 undefined | **三周后**——而且报错不指向这里 |
| 有没有守卫 | 有（`apimodel_test.go` 的 `TestSeedRoundTrip` 拿 seed.json 逐字节比对） | 基本没有（见 persistmodel README） |

「当场炸」听着更吓人，其实是**便宜**的那一类：反馈即时、责任明确、回滚立刻见效。
真正贵的是另一边那种静默腐烂。两者混在一个包里，就只能靠人记住哪个字段属于哪一类，
而这件事没有任何机制兜底。

## 依赖：只能 import 标准库

```
httpd → orchestrator/<域> → service/<域> → model/apimodel
                                        ↘ model/persistmodel → model/apimodel
```

本包是**叶子**。实测本包 21 个非测试文件只 import 了
`bytes` / `encoding/json` / `fmt` / `strconv` / `strings` / `time`，保持这样。

⚠️ **是 `persistmodel` 依赖 `apimodel`，不是反过来。** 这个方向由
`CloneAgent(apimodel.Agent) apimodel.Agent` 这类拷贝函数决定，改不了——
`model/persistmodel` 的 `clone.go` / `seed.go` / `persistmodel.go` 三个文件都
import 本包，而本包一个内部包都不 import。想反过来就是环。

## ⚠️ 领域规则跟着它的类型放同一个文件

`ValidIssueStatus` 放 `issue.go`，跟 `IssueStatus` 挨着；不要放进 `service/issue/`。

本包是 **117 个类型 + 25 个导出函数 + 6 个导出方法**，那 25 个函数
基本都是这种规则：`ValidIssueStatus`、`NormalizeFollowUpMode`、`IsValidEnvKey`、
`ValidSkillName`、`NormalizeRevisionReason`……

**这次重构要修的病，恰恰就是这类规则散在别处。** 不把这条写死，下一个人会很自然地
把 `ValidIssueStatus` 写进 service——它看起来像"业务逻辑"。但规则跟类型分家有两个
后果：一是同一个枚举会在两三个地方各判一遍且慢慢判得不一样，二是别的域想复用这个
判断就得 import 那个域的 service，而**域包之间是零 import 的**，于是只能抄一份。

判据：**这个函数只看这个类型自己的字段吗？** 只看 → 跟类型走。要读库、要调别的域、
要发事件 → 那才是 service 的东西。

## ⚠️ 必须是单一 package，不能因为文件多再切子包

一个实体一个文件（`agent.go` / `run.go` / `skill.go` / `issue.go`…），
但**全部都是 `package apimodel`**。

文件多起来之后，"再按域切成 `apimodel/agent/`、`apimodel/skill/`" 是个很自然的念头，
**但它不成立**：契约类型互相引用是常态——`Agent.SkillIDs` 指向 `Skill`、
`Run` 里带 `AgentID`、`Workspace` 里挂成员——切完这些就成了跨包引用，
而实体之间本来就有环（A 引 B、B 也引 A），Go 编译不过。

到时候只有两条出路：造一堆 `type ID = string` 的弱化字段（契约表达力当场退化），
或者建一个 `apimodel/common` 把互相引用的都塞进去（等于绕一圈回到单包，还多一层）。
**文件多不是问题，`ls` 一下就看完了；包切错要动几千处引用。**

## 往这里加类型

- 一个实体一个文件，文件名就是实体的小写名。
- 规则函数跟类型同文件（见上）。
- 搬动引用用 `gofmt -r`，**不要用 sed**——它是 AST 感知的，不误伤前缀重叠的标识符、
  不改注释里的同名词、不改 `type` 声明本身：

  ```bash
  gofmt -w -r 'Issue -> apimodel.Issue' ./core   # 作用范围必须排除本包
  ```

  它不会自动加 import，按编译报错补。

⚠️ **搬完必须比对 json tag 字面量集合**，这是「契约零漂移」的真判据：

```bash
git show HEAD:server/model/apimodel/apimodel.go | grep -oE 'json:"[^"]*"' | sort > /tmp/a
grep -oE 'json:"[^"]*"' server/model/apimodel/apimodel.go | sort > /tmp/b
diff /tmp/a /tmp/b        # 必须为空
```

比**字面量集合**，不比含 `json:` 的行——字段类型名变长会让 gofmt 重排整列，
diff 里一增一删但内容其实没变。

⚠️ **`omitempty` 的坑由本包 `apimodel_test.go` 的 `TestSeedRoundTrip` 兜着**：
它拿 seed.json 当 golden file，反序列化再序列化后逐字节比对，一次同时锁住
omitempty、null vs 缺失、空切片、时间戳三位小数四类。它按相对路径
`../persistmodel/seed.json` 找种子——种子随 `go:embed` 待在 persistmodel 里，
那个限制搬不动。

⚠️ **「切片 / 数字 / 布尔 / 指针不许带 `omitempty`」那三条铁律也已经有闸门**：
本包 `jsontag_test.go` 的 `TestJSONTagIronLaws` 用反射遍历全部导出结构体
（递归进嵌套结构体与切片元素）逐字段断言，违反就红。它与上面那个测试互补——
`TestSeedRoundTrip` 只覆盖**种子里出现过的**那几条数据，锁不住「新加一个字段、
种子里没有它」；这个反过来不看数据只看类型。

同文件的 `TestJSONTagGuardCoversEveryType` 做覆盖率对账（用 `go/ast` 数源码里的导出
结构体，与反射实际走到的集合比对），防的是**闸门自己静默失效**：遍历从一份手写的
根类型清单出发，加了新类型没人补一行的话，测试仍是绿的、但那个类型没人看着。
这两个测试已随类型搬进本包，根类型清单里是 `apimodel.Xxx{}`。

例外走 `allowedOmitEmpty` 白名单（当前 10 条，全是存量的刻意例外，每条在字段定义处
都有长注释）。白名单带反向对账，只减不增。
