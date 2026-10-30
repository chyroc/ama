# 产品建议（Suggestion）—— 平台级的意见与建议

**读它的时机**：改建议相关的任何东西之前；或者有人问「这跟 issue / feedback 是不是
一回事」的时候；或者要给一个**没有空间维度**的资源设计权限的时候。

代码：`core/suggestion.go`（Service + 全部授权）、`core/handler_suggestion.go`（HTTP）、
`repository/schema/core_suggestion_pg.sql`（三张表）、
前端 `web/src/pages/SuggestionsTab.tsx`、CLI `cmd/ama-cli` 的 `suggestion` 子命令。

## ⚠️ 它与 issues / feedback_reports 的区别（本文档最重要的一节）

**这是本仓库第三种「有问题」的东西。** 前两种已经因为名字像写过一整节文档去区分
（`README-issue.md`），第三种更需要——因为它同时长得像那两个。

|  | `feedback_reports` | `issues` | **`suggestions`** |
| --- | --- | --- | --- |
| 谁写的 | **机器采集**（前端 catch 自动落库） | 人手写 | **人手写** |
| 内容从哪来 | Service 按白名单**重新构造** | 用户输入，原样存 | **用户输入，原样存** |
| 归属 | 跨空间，`workspace_id` **可空** | **空间级**，非空 | **平台级，压根没有这一列** |
| 说的是什么 | 某次请求出错了 | 这个空间里一件待办 | **对 AMA 平台本身的意见** |
| 生命周期 | 一次性，去重合并 `occurrences+1` | 有状态流转、有评论 | **有状态、有回复** |
| 谁能读 | 只有开发团队（白名单，fail-closed） | 空间成员按角色 | **提的人看自己的 + 平台方看全部** |
| 用户能看到自己那条吗 | **不能**（只写不查、无句柄） | 能 | **能，且只能看自己的** |

### 「平台级」不是「workspace_id 可空」的另一种写法

这是最容易读错的一行。`feedback_reports` **有** `workspace_id` 列（出错时**碰巧**
在某个空间里，所以可空）；`suggestions` **根本没有这一列**——用户是在给平台提意见，
不是在给某个空间提意见。

留一个恒为空的列会让下一个人以为该按空间筛，然后写出一个永远返回空的筛选器。

### 合并进任何一张的后果

- **并进 `feedback_reports`** → 破坏它的核心不变量。那张表的价值全在「用户不知情时
  采集的每个字段都经过白名单」，往里加一个用户自由输入的正文列，等于在一张承诺过
  「绝不采集用户输入」的表上开自由文本口子。i18n 里那句 `feedback.neverCollected`
  **是承诺不是装饰**。
- **并进 `issues`** → `issues.workspace_id` 是 **not null**，而且那一列是**授权的
  依据**（authz 按它反解角色）。平台级的东西塞进去只能编一个假空间 id，那会让 Guard
  拿着一个不存在的空间判角色，结果是**谁都读不到、且报错说「空间不存在」**——
  一个完全不指向真实原因的故障。

## ⚠️ 为什么不叫 feedback

那个词在本仓库**已经被「自动错误采集」占了**：`feedback_reports` 表、`/api/feedback`
路由、`feedback.readers` 配置、i18n 的 `feedback.*` 一整片、前端 `lib/feedback.ts`
与 `FeedbackButton.tsx`。

再叫一次的代价不是命名洁癖，是**两套东西会在代码里、在对话里、在日志里长期混淆**：

- 代码里 `listFeedback` 与 `listFeedback`（另一个）只能靠包名或后缀区分；
- 对话里「反馈那个功能」从此有两个所指，每次都要追问一句是哪个；
- 日志里 grep `feedback` 会同时捞到两条链路。

而这个仓库**已经为「issue 与 feedback 名字像」付过一次这个代价**了（那一节文档就是
代价本身）。所以第三个换个词：**suggestion（产品建议）**。

候选里没选的两个也记一下，免得下次再纠结：`platform_feedback` 仍然含 feedback 那个词，
grep 不开、口语上还是会被叫成「反馈」；`voice` 太虚，看名字猜不出里面装的是什么。

**用词在三处保持一致**：库里是 `suggestions`，接口是 `/api/suggestions`，
界面上叫「产品建议」。⚠️ 界面文案里**不要出现「反馈」两个字**——那会把用户心智
拉回另一套东西上。

## 授权全在 Service 层，这是本仓库第三处

`authz/matrix.go` 里这 13 条**全是 `ActionNone`**。**那不是「忘了定级」**：
authz 的模型是「actor + 空间 → 角色」，而建议**没有空间维度**，Guard 表达不了它。

前两处是 `GET /api/workspace-by-key`（空间来自 query 参数）与 `GET /api/feedback`
（跨空间）。这一处的理由与后者同类但更彻底——那条至少还是「跨空间」，这里连
「碰巧属于某个空间」都没有。

规则一共两条：

```
提建议的人  读得到、改得动自己那些（含回复与附件）
平台方      读得到、改得动全部，且只有它能改状态
```

### ⚠️ ActionNone 让 Guard 完全不设防，所以 Service 漏一条就是一个洞

issue 那边即使 Service 漏判，Guard 还会按空间反解拦一道（那组有 `kind` + `idParam`）。
**这里没有那道保险**。附件下载那条尤其要紧：URL 会进 `<img src>`、会出现在分享的
链接里、浏览器历史里、日志里，漏判的话任何拿到 URL 的人都能看别人贴的截图，
而这条泄漏**不产生任何报错、也没有任何界面会显示它发生过**。

`httpd/suggestion_test.go` 用**真装配**钉住它，并且**先断言作者自己取得到**
——没有那一条的话，403 可能只是因为附件根本没传上去。

### 附件字节在对象存储，元信息在 PG

与 issue 附件同一条路（`shared/infra/filebytes`）：字节先写进 S3 兼容对象存储，
`suggestion_attachments` 只存元信息与 not null 的 `file_id`，**没有 `content` 列，也没有回落**。
下载**经 amad 中转**（`GET /api/suggestion-attachments/{id}`），所以上面那条判权仍然在
Service 里做——别为了省一跳改成给浏览器发对象存储的预签名链接，那样判权就绕过去了。
细节见 [README-issue.md](README-issue.md) 的「附件字节在对象存储」。

### ⚠️ 反过来也不行：别给这些路由加 kind / idParam

「顺手统一一下，让 Guard 也管一管」会让 Guard 拿建议 id 去 issues 表里反解，
恒定失败 → 每条都 404，而报错说「资源不存在」。而且 **`ActionNone` 的路由 Guard
直接透传，`kind`/`idParam` 写了会被静默忽略**——这个错误连启动时都发现不了。

## 三个设计判断

### 一、谁能看谁的：用户看自己的，平台方看全部

「平台方」**复用 `feedback.readers` 那份白名单**，不另开一个配置。

反对意见是真的（两件事的授权耦合了，以后想让「能看建议但不能看报错流」的人存在就得
拆开）。但当前**两者恰好是同一批人在做同一件事**：看用户报上来的东西、据此改平台。
分两份名单的真实后果是有人被加进一份忘了另一份，然后「他能看反馈却看不到建议」,
而那个故障零报错。

哪天真要拆，路径是清楚的：加一个 `suggestion.responders` 配置项、
`core.Config` 加一个字段、`isSuggestionStaff` 改读它——**一个函数一处改动**
（那个函数存在的意义就是这个）。

**留空即 fail-closed**：没有人是平台方。那时所有人仍能提建议、看自己那些，只是
没有人看得到全部、没有人能改状态。⚠️ 兜底成 true 的话「忘了配名单」等于
**每个人都是平台方**，任何人都能读到全公司提的所有建议——零报错、界面完全正常。

### 二、实名，但不公开展示

记 `author`（员工邮箱），**只从 context 取**，请求体里没有这个字段。

实名是回复与跟进的前提——匿名的话平台方回一句「能说说具体场景吗」没有人会看到。
而「有顾虑」那个担心由**不公开展示**解决：普通用户列表里只有自己那些，
作者列**只对平台方渲染**（`SuggestionsTab.tsx` 里那个 `isStaff ?`）。
所以不存在「被同事看见我提了什么」这件事。

### 三、不做去重，也暂不限流

**绝不加 `dedup_key`。** 人提的东西不该去重——两个人提同一个建议是两份独立的诉求，
合并会丢掉「有几个人想要」，而那正是判优先级的依据。这与 `README-issue.md` 那句
逐字相同。

⚠️ issues 那边有一个划了边界的例外（机器人产出的按 `origin`+`fingerprint` 合并），
**这里连那个例外都没有**，因为触发它的前提不成立：那条 partial 唯一索引是为
「定时巡检把同一批素材每 X 分钟重扫一遍」准备的，而建议全部由人手打出来。
`testSuggestionsAreNeverDeduped` 钉住这件事（三条逐字相同的建议必须都在）。

**频率限制暂时不做**，判据是「现在做等于凭空猜一个阈值」：这个功能还没上线，
真实提交速率是未知的，猜一个数的两种错法都不好（太松等于没做，太紧会把一个
认真写了五条建议的人挡在门外，而他正是最该被听见的人）。真要做的时候该按
「单位时间条数」限，**不是按内容合并**。

## 状态：两态，不是 issue 那样的三态

`open` / `resolved`。

issue 的 `in_progress` 有价值是因为报问题的人就在那个空间里等结果，「有人在看了」
与「还没人管」对他是两件事。建议不一样：提的人只想知道「这条被处理了没有」，
而平台方内部到了哪一步该由**回复正文**说清（回复能写「下个版本做」，一个枚举档位
写不了）。多一档没人维护的中间态只会让列表上一半条目永远卡在那里。

**加档位是廉价的**（同 issue：没建 check 约束，加一档不必迁移 DDL），
而减档位要动存量数据——所以先给两态。

### ⚠️ 认不出的状态一律拒，绝不默认改成 open

这条纪律抄自 `ValidIssueStatus`。静默纠正的后果：调用方拼错状态名
（`resolve` 少个 d、`Resolved` 大写），接口回 200 而那条建议的状态根本没变成他要的
那个——他以为改好了，而平台方看到的仍是「未解决」。

**只有平台方改得动状态**，作者本人改会 403。状态表达的是「平台方处理到哪一步了」，
让提的人自己点「已解决」会让平台方看列表时分不清「我们解决了」和「他自己觉得算了」。

## 回复：一张表，靠 is_staff 区分

平台方答复与用户追问走**同一张表、同一条路径**。分成两类的话「谁能回」这个判断
会散在两处，而它只有一条规则（能读这条建议的人就能回）。

### ⚠️ is_staff 是落库的真列，不是读取时现算的

现算（拿 author 去比当前白名单）的后果很隐蔽：白名单会变，有人调岗被移出名单之后，
他当年那条以平台方身份写的回复会**追溯性地**变成用户回复——整条历史对话的语义
都跟着漂。反过来也一样：新加进白名单的人，他之前以用户身份提的追问不该变成官方答复。

**写入那一刻他确实是平台方，那就是事实，事实不该随配置变。**
`SaveSuggestionReply`（两个实现都是）因此只更新 `body` 与 `updated_at`。

### ⚠️ 平台方回复不露邮箱，而且必须在服务端抹

平台方的回复是以**平台的名义**说的（「收到，下个版本做」），不是某个人的个人承诺。
`ListSuggestionReplies` 对非平台方**把 `author` 置空**（`hideStaffIdentity`）。

两个后果决定了这件事非做不可：

- **露出邮箱等于公布平台由谁开发**。那是组织信息，用户不需要，而且名单会随调岗
  变动——等于把内部人员构成挂在产品界面上。
- **承诺被挂到个人头上**。用户下次会直接私聊那个人催进度，绕过这条链路——
  而「有记录、可追溯、换人接手也不断」正是这个功能存在的理由。

**只在前端不渲染是不够的**：那个值仍然在 JSON 响应里，F12 一看就有、脚本抓一次
全拿到。这与本仓库那条「『不在界面上放入口』不是权限控制」是**同一个错误**。

三个方向要一起对，少一个都会往错的方向漂：

```
普通用户看平台方那条  →  author 空串（置空，不编假名——假名会被当成真地址渲染）
普通用户看自己那条    →  author 保留（一律抹会让对话像两个匿名者自言自语）
平台方看平台方那条    →  author 保留（内部协作要知道谁跟进的）
```

⚠️ 前端判据是**「有没有值」而不是「是不是平台方」**：写成 `!r.isStaff && ...`
的话平台方视角下反而看不见了。

**改与删的特权刻意相反**：平台方**改不了**别人的回复（那是篡改发言，要纠正就再回
一条），但**删得掉**（那是管理动作，清垃圾内容）。

## 为「以后让 agent 自动处理」留的路

`suggestion_replies.origin`：空串 = 人写的（**当前全部如此**），`agent` = 机器产出。
本次不写入非空值，只是把列和枚举备好。

⚠️ **与 `issues.origin` 用途不同，别照抄那边的推论**：那边 `origin` 进了唯一索引、
决定去重作用域；这里**不参与任何唯一约束**，只用来在界面上区分「这是机器人回的」。
看到有 `origin` 就顺手加一个 `fingerprint` 去重是错的（见上面「不做去重」）。

## 复用了 issue 的三样东西，都是刻意的

| 复用的 | 为什么不复制一份 |
| --- | --- |
| `DeriveIssueTitle`（标题提取） | 里面是一串**顺序敏感**的 Markdown 剥离规则（图片先于链接、粗体先于斜体、按 rune 截断）。复制出来的第二份会慢慢分叉，而分叉的表现只是「标题有时候怪怪的」，没人会去查 |
| `DetectAttachmentType` + `AttachmentDisposition`（三层防御） | 那套判定与实体无关。分叉出来的是**安全漏洞**——比如某一侧后来漏掉「嗅探没意见时不认 image/」那条守卫，于是任意字节改名成 `.png` 就能被 `<img>` 渲染 |
| `MaxAttachmentBytes` / `MaxAttachmentsPerIssue` | 同上，且两个上限不同会让用户困惑「为什么这里能传那里不能」 |

**没复用**的是 `AttachmentDropZone`：那个组件里写死了 `api.uploadIssueAttachment`。
改成收一个上传函数会动到 issue 那两个页面，收益不抵风险——建议这边只要一个按钮。

## Store：三张表，两个计数是查出来的

与 issue 那一域同构（全是真列、没有 doc jsonb，判据是「每个字段都在被查询」）。
`ReplyCount` / `AttachmentCount` 用子查询**现算，不落库**——存一份就要在每次增删时
同步更新，漏掉一处的表现是「列表说 3 条回复、点进去只有 2 条」，没有任何报错。

**排序**：建议列表 `order by ord desc`（最新在前），回复 `order by ord`（正序，
对话从上往下读）。两者相反是刻意的。用 `ord` 而不是 `created_at`：同一毫秒建的
两条按时间排顺序不稳定，表现是刷新一下列表里两行换了位置。

### ⚠️ 分页必须作用在筛选之后

**先切页再按 author 筛**是这一域最容易写反的地方：那种实现下，一个只提过 1 条建议的
用户，在库里有 5 条别人的建议排在他前面时，第一页会**完全是空的**——而接口回 200、
列表回 `[]`，没有任何报错。

SQL 那侧天然正确（`where` 先于 `limit`），内存版靠 `pageSlice` 在筛完之后切。
`testSuggestionAuthorFilterAppliesBeforePaging` 专门钉这件事，
**造数据时目标用户那条必须排在别人后面**，否则第一页天然包含它，错误实现照样绿。

### Save 不改 author

`author` 是这一域**授权的依据**（非平台方只读得到 `author` = 自己的那些）。
允许它被 Save 改掉等于开出一条越权路径：把自己的建议记到别人名下，或者反过来把
别人的据为己有。同 `SaveIssue` 不更新 `workspace_id`。

⚠️ **内存版第一版真的写错了这条**（整行覆盖），被
`testSuggestionSaveDoesNotChangeAuthor` 在 PGStore 与 MemStore 的对跑里抓出来——
那正是「Store 契约必须对两个实现各跑一遍」这条规矩存在的理由。

## 测试

```bash
cd server && go test ./core/ ./httpd/ ./authz/ -run 'Suggestion'
cd server && go test -run 'TestStoreSuite/.*/Suggestion' ./repository/dao/coredao/
```

Store 契约 11 条**对 MemStore 与 PGStore 各跑一遍**（`runStoreSuite`）：排序、
派生计数、字节隔离、分页四件事在两边由完全不同的机制实现（SQL vs Go），
只在一边验的话另一边的分叉没人看着——而单测跑的正是内存版。

**每条安全断言都做过变异验证**（安全校验没有变异验证等于没验）：

| 变异 | 变红的用例 |
| --- | --- |
| 附件下载不判权 | `TestSuggestionAttachmentIsPrivateToAuthor` |
| 列表不按 author 收窄 | `TestSuggestionListIsScopedToAuthor` |
| 单条读不判权 | `TestSuggestionIsPrivateToAuthor` |
| 作者也能改状态 | `TestSuggestionStatusOnlyStaffCanChange` |
| 认不出的状态静默改成 open | `TestSuggestionUnknownStatusIsRejected` |
| 回复的 `isStaff` 恒为 true | `TestSuggestionRepliesAreTwoWay` + `...CannotBeForged` |
| 回复不判权 | `TestSuggestionRepliesAreTwoWay` |
| MemStore 的 Save 覆盖 author | `TestStoreSuite/MemStore/SuggestionSaveDoesNotChangeAuthor` |
| PGStore 的 Save 也 set author | `TestStoreSuite/PGStore/...`（同名，PG 那遍） |
| 内存版不分页 | `TestStoreSuite/MemStore/SuggestionPaging` |
| 平台方邮箱不抹（退回泄漏） | `TestSuggestionStaffIdentityIsHiddenFromUsers` |
| 一律抹（连用户自己那条） | 同上 |
| 平台方视角也抹 | 同上 |

## 本地怎么把自己变成「平台方」

`server/config.yaml` 里配 `feedback.readers`（**同一份名单管两件事**）：

```yaml
feedback:
  readers:
    - 你@example.com
```

判据是**启动日志**，它两个方向都会说：

```
未配置问题反馈读取白名单（feedback.readers）——  ← 没有人是平台方
问题反馈读取白名单已配置 count=1 readers=[...]   ← 名单生效了
```

不看日志的话，「建议列表里只有自己那两条」会被读成功能坏了，而它其实是正确行为。

## 已跑通的闭环（真库、真服务、真 CLI）

```
提一条建议（标题留空 → 从正文提取成「希望对话页支持一次发多张图」）
  → 传一张真 PNG（服务端嗅探出 image/png，中文文件名按 RFC 5987 编码）
  → 按 URL 取回，字节逐字相同，三个安全头都在
  → 平台方回复（isStaff=true 落库）
  → 只传 status 改成 resolved（标题正文没被清空，两个计数变成 1/1）
  → ama api GET /api/suggestions /api/suggestions/<id>/replies 读得出来
    （公开 ama CLI 刻意不封装这组命令——它只给平台方用，普通 agent 走 api 逃生口）
  → 把自己从白名单摘掉重启：viewerIsStaff 变 false、改状态拿 403、
    但自己提的两条仍然看得到  ← fail-closed 那一支不是恒真的
```
