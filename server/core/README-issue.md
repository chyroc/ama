# 问题（Issue）—— 空间级的问题记录

**读它的时机**：改 Issue 相关的任何东西之前，尤其是要动附件那条路由的时候；
或者有人问「这跟 feedback 是不是一回事」的时候。

代码：`core/issue.go`（Service）、`shared/gopkg/mdtitle/mdtitle.go`（标题提取）、
`shared/gopkg/attachment/attachment.go`（类型判定与响应头）、`core/handler_issue.go`（HTTP）、
`repository/schema/core_issue_pg.sql`（三张表）、前端 `web/src/pages/Issues*.tsx`。

## 这个功能解决什么

空间里第一次有了「记录一件待处理的事」的地方：人写的标题与正文、能贴图和附件、
能讨论、有状态流转与优先级。

## ⚠️ 它与 feedback_reports 不是同一件事，不要合并

这是本文档最重要的一条，因为两者名字像、领域也像（都是「有问题」）。
**四条轴上正好相反**：

> ⚠️ **现在是三个了。** 又加了一个 `suggestions`（产品建议）——人手写、
> **平台级**（那张表连 `workspace_id` 列都没有）、提的人看自己的 + 平台方看全部。
> 三者的完整对照见 [README-suggestion.md](README-suggestion.md) 第一节。
> 下面这张表只讲前两个。

| | `feedback_reports` | `issues` |
| --- | --- | --- |
| 谁写的 | **机器采集**（前端 catch 里自动落库） | **人手写**的标题与正文 |
| 内容从哪来 | Service 按白名单**重新构造** | 用户输入，原样存 |
| 归属 | **跨空间**，`workspace_id` 可空 | **空间级**，非空 |
| 生命周期 | 一次性，去重合并成 `occurrences+1` | 有状态流转、有评论、会被改 |
| 谁能读 | 只有开发团队（邮箱白名单，fail-closed） | 空间成员按角色 |
| 用户能看到自己那条吗 | **不能**（只写不查、无句柄） | 能查能改能跟进 |

**合并会破坏 feedback 的核心不变量。** 那张表的价值全在「用户不知情时采集的每个
字段都经过白名单」（见 `README-feedback.md`，有变异验证钉着）。往里加一个用户自由
输入的正文列，等于在一张承诺过「绝不采集用户输入」的表上开自由文本口子——
而 i18n 里那句 `feedback.neverCollected` **是承诺不是装饰**。

**反过来把 feedback 塞进 issues 也不行**：它靠 `dedup_key` 唯一索引限流（一个坏接口
一分钟能刷上百条），而**人报的** issue 恰恰**不能去重**——两个人报同一件事就该是两条。

### ⚠️ 「不能去重」有一个划了边界的例外：机器人产出的

`origin` + `fingerprint` 两列 + 一条 **partial** 唯一索引
（`where origin <> '' and fingerprint <> ''`），代码在 `core/issuebot.go`。

**这没有推翻上面那句话**，因为那句话针对的是**人**：两个人报同一件事是两份独立的
证词，合并会丢掉「有几个人受影响」——而那正是判优先级的依据。机器人是另一回事：
一条定时巡检任务每 X 分钟把**同一批素材**重扫一遍，问题没修之前每轮都会重新发现它。
不合并的结果不是「多几份证词」，而是几百条逐字相同的记录把列表淹掉，
**人报的真问题反而找不到了**。

边界由数据保证，不靠约定：

| `origin` | 谁 | 去重 |
| --- | --- | --- |
| 空串（**存量全部如此**） | 人手写 | **永不**——partial 索引的 `where` 压根不覆盖它 |
| `schedule` | 定时巡检 | 同空间 + 同指纹 → 合并成一条 + 追一条评论 |

`origin` 进了唯一键，所以两种来源**永远不互相合并**。
**没有给 issues 加任何全局唯一索引**——这一点是这个设计成不成立的关键。

三条容易写错的：

- **指纹只由 agent 给的 `key` 决定，不含正文、不含时间。** 正文里几乎必然有每轮都变
  的东西（次数、时间戳、pod 名），算进去等于不去重——而那个故障不报错，
  只是列表慢慢堆满几乎一样的记录。
- **`FindIssueByFingerprint` 对空 origin/指纹一律返回 ErrNotFound。** 放开的话，
  机器人的评论会被追到某条人报的问题上，而那条问题的作者完全不知道为什么。
- **判重是两层：先查一次（优化）+ Store 的唯一约束（正确性）。** 多实例下只有前者
  是 TOCTOU。⚠️ 做变异验证时**必须两层一起去掉**才会变红——只去一层是 no-op，
  那不代表用例没验住。

已对**有存量数据的老库**实跑过迁移（两遍，验幂等）：两条同名的人手写记录都还在。

**目前也不留 `source_feedback_id` 连接列**。以后真要做「把一条 feedback 转成 issue」
再 `alter table issues add column if not exists`（本仓库有先例，`schema_pg.sql:293`），
现在加一个零调用方的列只是噪音。

## 归属是空间级

`workspace_id` 非空，集合路由挂 `/api/workspaces/{workspaceId}/issues`。三条理由：

1. issue 描述的是**这个空间里**的问题，换个空间看到同一批是错的心智模型；
2. **挂空间下 Guard 才管得到**——路径里有 workspaceId，authz 直接判角色，
   不必把校验下移到 Service（那是 `authz/README.md` 明确要避免的）；
3. **附件的越权防护因此自动成立**：附件继承 issue 的空间，Guard 按附件反解。

对照：`integrations` / `user-preferences` / `global-ark-credential` 之所以是
`ActionNone`，是因为它们的对象不是空间资源、越权表达不出来。Issue 不属于那一类。

## 标题非必填，从正文提取；两者都空则拒

规则在 `shared/gopkg/mdtitle/mdtitle.go`，前端有一份**逐条一致**的实现
（`web/src/lib/issueTitle.ts`）——LocalApiClient 是契约真源，没有那份的话
「不填标题」这条路在 e2e 里根本走不到。

```
标题非空          → 原样用（trim + 压空白 + 截断）
标题空、正文非空   → 取正文第一个「有内容的行」
两者都空          → 返回空串，由调用方拒绝（400）
```

提取时逐行扫描（不是只看第一行），跳过空行 / 围栏行 / 分隔线，然后剥：
ATX 标题（`# ` 要求有空格，所以 `#紧急` 不受影响）、引用、列表前缀、
行内强调与代码、删除线、**图片与链接取替代文字**，最后按 **rune** 截断到 80。

**兜底是「未命名问题」，不能留空**：空标题在列表里是一行看不见的东西，
用户会以为数据坏了、或者以为没提交成功然后再提一遍。

### ⚠️ 三条最容易写错的

**一、`DeriveIssueTitle` 在两者都空时返回空串，刻意不兜底。** 那是一条什么内容都
没有的记录，应该被拒；在提取函数里兜底会让空记录静默进库。兜底只发生在
「正文有内容但剥完为空」那一种情况。

**二、剥离顺序不能换。** 图片必须先于链接（图片语法是链接前面加个 `!`，
反过来会剩下 `!alt`）；粗体必须先于斜体（`**x**` 按单星号匹配会剩下 `*x*`）。

**三、按 rune / 码点截断，不按 byte / UTF-16 码元。** Go 那侧按 byte 截会把汉字切成
半个（落库是 U+FFFD）；前端用 `slice()` 会把 emoji 切出孤立代理项。
这类损坏在测试里看着只是「标题短了点」。

### 一条「测了但没测住」的实例，值得记下

第一版 e2e 用的是**没有 alt** 的图片 `![](url)`。做变异验证时注掉「剥图片」那一步，
**用例仍然是绿的**——因为剥完只剩一个 `!`，没有可读字符，于是被
「这一行没内容」那条**另一条**规则跳过了，标题照样正确。

也就是说那条用例验的不是它以为在验的东西。补了一条带 alt 的
（`![登录页截图](url)`）才把两条规则分开：不剥的话链接规则把它变成 `!登录页截图`，
那**有**可读字符，标题真的会坏。

**教训**：设计安全/正确性用例时要问「这个输入能不能被另一条规则顺手救回来」。
能的话它就区分不出你想验的那一条。

## 状态与优先级：枚举，不用布尔；默认 P2 不是 P0

`open / in_progress / closed`，`p0 / p1 / p2`。

**用枚举不用 `closed bool`**：将来加一档不必改表结构。`in_progress` 有它的价值——
「有人在看了」与「还没人管」对报问题的人是完全不同的两件事。

**不填优先级落 P2**。不默认 P0 的理由很实在：默认最高档意味着每条新问题都在喊
「最紧急」，于是 P0 这一档立刻失去意义——真正的 P0 淹没在一片 P0 里。
界面上 P2 也是中性色（P0 才是 danger），否则视觉上同样是「每条都在喊」。

**两个枚举都不建 check 约束**：值由 Service 校验（认不出一律拒），
check 只会让以后加一档变成一次 DDL 迁移。同 `feedback_reports.level`。

## 附件字节在对象存储，元信息在 PG

**字节只在 S3 兼容对象存储里**（`shared/infra/s3`，compose 里是 RustFS），PG 只存元信息。
`issue_attachments` 没有 `content` 列，`file_id` 是 not null。理由：

- **绝对不能存本地盘。** 多实例部署下每个实例的磁盘各自独立且重建即丢；
- 也不放 PG：字节进库会让库体积与备份跟着附件涨，每次读写都要穿过连接池搬几 MB。

上传走 `core.BlobStore`：先把字节写进对象存储、再落一条 files 记录（file_id 写进
issue_attachments）；读取按 file_id 找到 files 行，再去对象存储取（`filebytes.Store.Read`）。
**没有任何回落**：对象没了就如实报错（`ErrBlobNotFound`），没装配 BlobStore（只在单测）
时上传回 422（`filebytes.ErrNotConfigured`）。PG 档没配对象存储时 amad 直接拒绝启动。

下载仍然**经 amad 中转**（`GET /api/issue-attachments/{id}`），不给浏览器发对象存储的
预签名链接——判权要在 amad 里做（见下一节），而且这样附件链接与对象存储地址无关。

完整论证在 [`repository/schema/files.sql`](../repository/schema/files.sql) 的文件头。

## ⚠️ 附件 URL：看着像静态资源，但一个字都没放宽

`GET /api/issue-attachments/{id}`，matrix 里是
`{action: ActionRead, kind: KindIssueAttachment, idParam: "id"}`。

**凭据走 Cookie 那条通道**：`<img>` 请求不带自定义头，但同源会自动带上 HttpOnly
Cookie（`ama_session`，`Path=/`、`SameSite=Lax`），而 `auth.credentialFrom` 的三条
通道里正好有它——所以前端一行代码都不用写。

**定成 `ActionNone` 的后果**：任何拿到 URL 的人都能看**别人空间**的图。
URL 会出现在分享的链接里、浏览器历史里、日志里。而这条泄漏**不产生任何报错、
也没有任何界面会显示它发生过**。变异验证时非成员真的拿到了 `200` + PNG 字节。

`httpd/issue_test.go` 的 `TestIssueAttachmentIsWorkspaceScoped` 用真装配钉住它，
并且**先断言空间成员能取到**——没有那一条的话，403 可能只是因为附件根本取不到。

## 上传是不可信输入：三层防御

```
1. 类型白名单 + 服务端嗅探   挡住「上传 HTML 伪装成 png」
2. X-Content-Type-Options    挡住浏览器无视我们设的类型自己猜
3. Content-Disposition       非图片一律 attachment，不在浏览器里渲染
```

只做第一层不够：类型判定永远可能被绕，而后两层是无条件生效的。

**类型由 `http.DetectContentType` 嗅探决定，绝不用客户端声明的那个**，并要求
嗅探结果与扩展名**一致**——「内容是 A、名字说是 B」正是攻击的形状，
取哪一个都是在猜，猜错一次就是一个 XSS。

**svg 明确不收。** 它能内嵌 `<script>`、能用 `<foreignObject>` 塞 HTML，
而 `image/svg+xml` 的响应即使带 nosniff 也会被当矢量图渲染、脚本照跑，
跑在我们的同源里。唯一安全的收法是重新解析并剥掉脚本节点，代价与收益不成比例。

### 嗅探的两条反直觉事实

**一、`text/plain` 与 `application/octet-stream` 是「没有意见」，不是判定。**
`.md` / `.csv` / `.json` 嗅探一律回 `text/plain`（它不解析结构）。把这当成
「判定为 .txt」去做冲突检查的话，传个 `.json` 会被告知「内容看起来是 text/plain，
与扩展名 .json 不符」——用户完全无法理解。`sniffHasNoOpinion` 就为这件事存在。

**二、「嗅探没意见时不认 image/ 类型」那条守卫是真防线，别当多余删掉。**
它挡的是「把任意字节改名成 .png」：那种文件嗅探回 octet-stream，
放行成 image/png 就会被 inline 送给 `<img>` 渲染，而我们对内容一无所知。
`TestSniffWithoutOpinionNeverYieldsImageType` 单独钉它——因为
`TestRejectsSVG` 那条在白名单里没有 svg 时靠「不在白名单」就已经通过了，
看不出这条规则有没有生效。

**顺带**：裸 `<svg>` 嗅探回 `text/plain`，带 `<?xml?>` 声明的回 `text/xml`——
两种写法走**不同分支**，所以两条都要测。

### 文件名要按 RFC 5987 编码，且清理后再进响应头

中文文件名直接写进 `filename=` 会乱码（那个字段只认 ASCII），所以同时给
`filename*=UTF-8''<pct-encoded>`。

**清理是必须的**：文件名是用户输入而它要进 HTTP 头——带换行的能注入额外响应头
（CRLF 注入），带引号的能提前闭合 `filename="..."`。Go 的 net/http 确实会拒绝带
换行的头值，但依赖它意味着攻击的表现是 500 而不是「被正确处理」。

## Store：三张表，两个计数是查出来的

与 skills / runs 那套「标量列 + doc jsonb」**不同，这一域全是真列**——
判据还是那条「能查询/能聚合的拎成真列」，而 issue 的每个字段都在被查询，
doc 会是空的。

`CommentCount` / `AttachmentCount` 用子查询**现算，不落库**（同 `Run.AgentDeleted`
的取向）。存一份就要在每次增删时同步更新，漏掉一处的表现是「列表说 3 条评论、
点进去只有 2 条」——没有任何报错，且两个数字看着都合理。

`ListIssueAttachments` / `GetIssueAttachment` **刻意不 select `content`**：
一条问题可能挂 20 个附件、每个 5 MiB，列表里带上字节会让一次页面加载搬运 100 MB。
授权反解也走不带字节的那条（每张图请求都要反解一次）。

**排序**：问题列表 `order by ord desc`（最新在前），评论 `order by ord`（正序，
对话从上往下读）。两者相反是刻意的。用 `ord` 而不是 `created_at`：
同一毫秒建的两条按时间排顺序不稳定，表现是刷新一下列表里两行换了位置。

## 测试

```bash
cd server && go test ./core/ ./httpd/ ./authz/ -run 'Issue|Attachment|Title|Sniff'
cd server && go test ./shared/gopkg/attachment/ ./shared/gopkg/mdtitle/   # 类型判定与标题提取
cd web && npx playwright test e2e/17-issues.spec.ts
```

⚠️ **附件类型判定与标题提取已经不在 core 了**：它们是 issue 与 suggestion 两个域
共用的东西，住在 `shared/gopkg/attachment/` 与 `shared/gopkg/mdtitle/`（连同各自的测试）。
下面表里最后四条变异要改那两个包的源文件，不是 `core/`。

Store 契约 7 条**对 MemStore 与 PGStore 各跑一遍**（`runStoreSuite`）：
排序、派生计数、字节隔离三件事在两边由完全不同的机制实现（SQL vs Go），
只在一边验的话另一边的分叉没人看着——而单测跑的正是内存版。

**每条安全断言都做过变异验证**（安全校验没有变异验证等于没验）：

| 变异 | 变红的用例 |
| --- | --- |
| matrix 里附件路由改 `ActionNone` | `TestIssueAttachmentIsWorkspaceScoped`（非成员拿到 200 + PNG） |
| matrix 里漏声明附件路由 | 启动 panic，报错点名那条路由 |
| 删掉 `nosniff` 头 | `TestIssueAttachmentSecurityHeaders`（2 条） |
| `Content-Disposition` 全设 inline | 同上（2 条） |
| `Content-Type` 写死 json | 同上 |
| 去掉「标题与正文都空就拒」 | `TestIssueRequiresTitleOrBody`（3 条） |
| 作者不取自登录身份 | `TestIssueAuthorCannotBeForged` |
| 默认优先级改成 P0 | `TestIssueTitleDerivedFromBody` |
| 嗅探与扩展名的一致性检查失效 | `TestDetectAttachmentTypeRejectsMismatchedExtension` |
| 删掉「嗅探没意见时不认 image」 | `TestSniffWithoutOpinion…` + `RejectsArbitraryBytes`（4 条子测试 + 1 条顶层） |
| `sanitizeFilename` 变恒等 | `TestAttachmentDispositionResistsHeaderInjection`（7 条） |
| `stripMarkdown` 变恒等 | 标题提取 29 条子测试（含兜底那 3 条） |
| 前端不剥图片语法 | e2e「标题取 alt」那条（**只有带 alt 的那条会红**，见上文） |

⚠️ 做「作者」那条变异时第一次写坏了：把 `actor` 换掉之后它变成未使用变量、
**编译失败**——那会伪装成「变异生效」而测试其实一次都没跑。
补一个 `_ = actor` 才算数。**变异必须能编译才算数。**

把上面最后四条搬进叶子包时又踩了同一个坑的另一种形状：`gofmt -r` 把测试文件里的
`ErrInvalidArgument` 改成了 `storemodel.ErrInvalidArgument` 但**不会加 import**，
于是整个测试包 `[build failed]`。那一刻「HTML 伪装成 png 要被拒」这条防线是空的，
而 `go test` 的输出里它和「跑过且通过」只差一行字。
所以变异脚本里先跑一次 `go test -run ZZZNOSUCHTEST`（只编译、不跑用例）当闸门，
编译不过就直接判这条变异不算数——**别让编译失败混进「变异生效」里**。
