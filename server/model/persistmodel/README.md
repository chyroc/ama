# model/persistmodel —— 只给持久化用的类型

`Store` 接口的入参、那些**不进 HTTP 响应**的行结构体，以及 PG 与内存两个实现共用的
拷贝函数。

> **类型已经搬进来了（第 18 步）。** 原先住在 `internal/storemodel`，现在是本目录的
> `clone.go` / `errors.go` / `persistmodel.go` / `seed.go`，`internal/storemodel` 已删除。
> 那 14 个 json tag 逐字节比对为空。
>
> ⚠️ **`seed.json` 连着一起搬了，这是硬约束**：`go:embed` 不能跨父目录（语言层面的
> 限制），它必须与 `seed.go` 同目录。`apimodel_test.go` 的 `TestSeedRoundTrip` 按
> `../persistmodel/seed.json` 这个相对路径读它，`scripts/export-seed/export.sh`
> 的 `OUT=` 也指着这里——动目录要一起改这两处。

## 判据：会不会进 HTTP 响应

**不是「有没有 json tag」。这两条不等价，本包自己就是活证据。**

本包现有 **14 个 json tag，一个都不是 HTTP 契约**：

| 类型 | tag 数 | 那些 tag 其实是什么 |
| --- | --- | --- |
| `ArkBinding` | **9** | `ark_bindings.doc` 这个 **jsonb 列**的格式（`marshalDoc` 用），handler 碰不到 |
| `seedShape` | 5 | `seed.json` 这个**文件**的顶层结构，未导出 |

所以按「有没有 tag」分类会把 `ArkBinding` 分错，而分错它的代价见下一节。

反过来，**没有 tag 也照样归这里**：`RunQuery` / `UsageQuery` / `EventQuery` /
`FeedbackQuery` / `SuggestionQuery` 五个（**各 0 个 tag**）在 handler 里只被
**构造成 Store 入参**，没有任何 `writeJSON` 吐它们。

判据只有一句：**handler 会不会把它 `writeJSON` 出去？** 会 → `apimodel`；
不会但要落库 / 要进 Store 签名 → 这里。

## ⚠️ 改错这里的后果：线上存量数据静默腐烂

这是本包与 `apimodel` **必须分开**的真正原因——两类的失败模式相反：

| | apimodel | persistmodel（本包） |
| --- | --- | --- |
| 改错了谁受影响 | 前端 | 线上存量数据 |
| 什么时候发现 | **当场**（白屏、字段 undefined） | **三周后**，且报错不指向这里 |
| 回滚有没有用 | 有，改回去就好 | **没有**，已经写进库的行改不回来 |

`ArkBinding` 那 9 个 tag 改一个键名的完整后果链：

```
存量行反序列化出空串 → 指纹对不上 → 重建 Environment → 撞 409
  → 那个空间再也开不了新对话
```

**全程零报错。** 而且这条链上没有一环会提到「你改了个 json tag」。

**更要命的是没有任何测试锁住它们。** 实测把 `skillsFingerprint` 改成
`skillsFingerprintX`，`go test ./core/ -run TestStoreSuite/PGStore` **照样全绿**
（62 条子用例都过）——因为那套用例是 `Put` 完再 `Get`，同一份 tag 序列化又反序列化，
**自洽但不锁形状**。所以「PG 用例全绿」不能作为「doc 格式没动」的证据。

唯一的防线是人工逐字节比对：

```bash
git show HEAD:server/model/persistmodel/persistmodel.go | grep -oE 'json:"[^"]*"' | sort > /tmp/a
grep -oE 'json:"[^"]*"' server/model/persistmodel/persistmodel.go | sort > /tmp/b
diff /tmp/a /tmp/b
```

比 tag **字面量集合**，不比含 `json:` 的行——字段类型名变长会让 gofmt 重排整列。

> 已知缺口：值得补一个 doc 形状的 golden 测试（照 `model/apimodel/apimodel_test.go`
> 拿 seed.json 逐字节比对的做法）。还没做。

## 依赖：只能 import 标准库与 model/apimodel

```
service/<域> → model/persistmodel → model/apimodel
```

⚠️ **方向是 persistmodel → apimodel，不是反过来，这一条改不了。**
`CloneAgent(apimodel.Agent) apimodel.Agent` 这类拷贝函数决定的：本包的
`clone.go` / `seed.go` / `persistmodel.go` 三个文件都 import `model/apimodel`，
而 apimodel 只 import 标准库。想反向就是环。

**不能 import `core`、不能 import `service/`、不能 import `infra`。**
本包存在的意义就是让「`Store` 接口的签名只依赖叶子包」成立——这些类型原先声明在
`core` 里，于是任何想实现 `core.Store` 的包都被迫 `import core`，**不是为了接口，
是为了几个行结构体**。

## ⚠️ 领域规则跟着它的类型放同一个文件

与 `apimodel` 同一条规矩：只看自己字段的判断/归一函数，跟类型挨着放，不要外移到
`service/`。要读库、要调别的域的，那才是 service 的。

本包尤其有一类容易放错的——**两个实现共用的拷贝/归一点**。`CloneSkill` 里那句
「`Files` 为 nil 就保持 nil，不补成空切片」必须待在这儿：`Skill.Files` 带
`omitempty`，补成空切片会序列化出 `"files":[]`，而种子里没有这个键，
`TestSeedRoundTrip` 当场变红。这类规则挪进 `service/` 就等于漏掉另一个实现，
两个实现对「空值」给出不同答案。

## ⚠️ 「中间结构」子包现在不要建

既不进 HTTP 响应、也不落库的**过程态**——本次盘过，**一个都没有**。
上面那 5 个 `*Query` 看着最像，但它们是持久化入参，归本包。

**空目录比没有更坏：它会吸引乱放。** 一个叫 `model/internal` 或 `model/dto` 的空
壳立在那儿，下一个人拿不准的东西就往里扔，两个月后它变成第三个语义不明的类型堆，
而这次重构的全部价值就是让「这个类型属于哪一类」有唯一答案。

真出现了再建，判据写死在这里：**既不进 HTTP 响应、也不落库的过程态。**
两个条件都要满足；只满足前一个的是本包的东西。

## ⚠️ 两个刻意的例外，别"顺手"搬过来

**`ArkBindingKey`（在 `core/chat.go`）**：它在 `Store` 签名里出现 **0 次**，属于
`ArkRunner`（访问方舟的**出站**端口），而且带 `APIKey string` 明文字段。
那是**凭据载体，不是存储记录**——放进一个名叫 persistmodel 的包，等于把
「明文 key 属于存储层」这个错误暗示钉进包名里。

**`FeedbackPage`（在 `apimodel`）**：响应体，见上表。

## ⚠️ 成员是「关联表」而不是 jsonb，加字段要改四处

`apimodel.WorkspaceMember` 的真源是 `workspace_members` **表**，不是 `workspaces.doc`
（那条 `workspace_members_subject_idx (workspace_id, subject_type, subject_id)`
唯一索引要求它独立成表）。

它现在有 6 个字段：`ID` / `Name` / `Email` / `Role` / `AvatarURL`（omitempty）/
`JoinedAt`。

给它加字段时，**改结构体是不够的**——`repository/dao/coredao/store_pg.go` 里有
四处要跟着改：

```
memberSelectColumns    select 列表
fillMembers            rows.Scan
insertMembersTx        insert，建空间与装种子走它
syncMembersTx          insert + update + delete，以及变化判据
```

**四处任何一处漏掉都不编译报错**，只会让字段静默丢失。第四处的变化判据尤其容易漏：
那是 `syncMembersTx` 里的 `case cur.role != string(m.Role) || cur.name != m.Name ||
cur.avatar != m.AvatarURL`——漏了它，改这个字段会被当成「没变化」而不 update，
**保存成功、值没变、零报错**。

守卫是 `coredao` 的契约用例 `MemberFieldsSurviveRoundTrip`（`store_test.go`），
由 `runStoreSuite` **对内存与 PG 两个实现各跑一遍**。加了字段就往那条用例里加断言，
否则等于没有守卫。

⚠️ **成员是「关联表」而不是 jsonb，这在新规则下已经是默认，不再是例外。**
本包仍有一批类型存整体 jsonb doc（`ArkBinding` 等 10 张表），那是**存量**；
新表按「字段展开成列、一对多用关联表」建，见
[repository/schema/README.md](../../repository/schema/README.md) 的
「表结构设计的默认规则」。所以上面那四处要改的地方，将来会是常态而不是特例——
而**列漏一个是编译错误或 42703，doc 漏一个是三周后的线上事故**（上面那条
`ArkBinding` 后果链就是后者）。

## 往这里加类型

- 搬动引用用 `gofmt -r`（AST 感知，不误伤前缀重叠的标识符）：

  ```bash
  gofmt -w -r 'ArkBinding -> persistmodel.ArkBinding' ./core   # 排除本包
  ```

  它不会自动加 import，按编译报错补。
- **连带要搬的容易漏**：类型的方法（如 `ArkSkillUpload.Ref`）、以及常量——常量未必
  带类型名前缀（`FeedbackLevel` 的取值叫 `FeedbackAuto` / `FeedbackUser`，按类型名
  搜整个搜不到）。好在这两类漏掉都是编译错误，不会静默。
- ⚠️ **碰 Store 的改动必须起 PG 再跑测试**：`docker compose up -d`。
  连不上库时那几个包整组 `t.Skip`，**跳过时是绿的**。
