# repository/schema —— 全部 SQL/DDL 集中在这里

**52 张表，一表一文件，文件名就是表名。** 找 `runs` 表的定义就打开 `runs.sql`。

**每份 `.sql` 顶部有一段「表结构一览」**，列全了字段、类型、not null / default 与索引，
不用读完整个文件就能看懂表长什么样：

```
-- ┌── Table outline: runs ─────────────────────────────────
-- │ id            text                      not null
-- │ started_at    text                      not null
-- │ ord           bigint                    not null (sequence)
-- │ schedule_id   text
-- │ …
-- │ primary key runs_pkey
-- │      (id)
-- │ index runs_workspace_started_idx
-- │      (workspace_id, started_at DESC, ord)
-- └───────────────────────────────────────────────────────
```

⚠️ **那段是生成的，不要手改。** 真源是**真库的 catalog**，不是 `.sql` 的文本：
脚本把全部 `.sql` 按 `schema.go` 的顺序灌进一个临时 schema，读完 catalog 再删掉。
改完 DDL 重新生成：

```bash
python3 scripts/gen-schema-outline.py     # 幂等，可反复跑
```

它只要求本地 PG 起着（`docker compose up -d postgres`）。**临时 schema 每次现建现删**，
不读常驻 schema——常驻的那种只在有人记得重灌时才跟得上 `.sql`，会**静默滞后**。
默认通过 `docker exec <compose 里 postgres 服务的容器> psql` 连库，容器名 / 库 / 用户可用
`AMA_SCHEMA_PG_CONTAINER` / `AMA_SCHEMA_PG_DB` / `AMA_SCHEMA_PG_USER` 覆盖。

带 doc 列的表在结构一览下面还有一段「doc 列的键」，由 `go run ./cmd/gen-doc-outline -w`
从 Go 类型的 json tag 生成，同样不要手改。

**每张表逐字段的理由写在结构一览下面**，那些注释是一手资料（几乎每条都是踩过的坑），
别只看列名。

## 表清单

| 归属 | 表（= 文件名） |
| --- | --- |
| `core` 六个资源域 | workspaces / workspace_members / agents / skills / skill_files / ark_secrets / ark_bindings / ark_skill_uploads / runs / run_events |
| `core` 其它 | schedules / entity_revisions / user_onboarding / feedback_reports / user_preferences / global_ark_secrets / agent_ark_secrets / agent_sync_tasks / terminal_tickets / selfhost_tool_results / run_sandbox_sessions / ark_auth_failures / pending_messages / mq_messages |
| `core` 问题（Issue） | issues / issue_comments / issue_attachments |
| `core` 产品建议 | suggestions / suggestion_replies / suggestion_attachments |
| `core` 文件 | files（附件与技能文件的元信息）/ user_files（账号级个人文件的元信息） |

**PG 不存任何文件字节**：字节在 S3 兼容对象存储里（`shared/infra/s3`），这些表只记桶、key、
大小、哈希。issue / suggestion 附件表也没有 `content` 列，只有 not null 的 `file_id`。
⚠️ 建表是 `create table if not exists`，**不改已存在的表**：本地老库（附件表还有 `content bytea`
列、还有那张存字节的旧表的那版）要 `docker compose down -v` 重建，或手动 drop 那两列 / 那张表。
| `core` 飞书用户 OAuth | lark_user_tokens / lark_user_auth_sessions / lark_user_app_prefs |
| `core` 自带机器执行端 | hosts / workspace_hosts / host_dispatches |
| `auth` | auth_sessions / auth_oidc_states / auth_github_flows / auth_api_keys / users / auth_local_accounts / auth_login_failures |
| `feishu` | feishu_apps / feishu_sessions / feishu_inbound_messages / feishu_inflight_turns / feishu_progress_deliveries / feishu_chats / feishu_session_scopes |

真源是 `schema.go` 里那三个 Order 列表，不是这张表。

「这一组为什么是一组」写在 `schema.go` 对应的分段注释里，讲的是共同设计
（core 那套 jsonb、auth 为什么不用 jsonb、issues/suggestions/feedback 三者为什么不该合并），
**不属于其中任何一张表**。

⚠️ 读那几段之前先读下面这节：**core 与 feishu 那两段讲的是带 doc 的既有形状，
不是现行规则**，别照着它给新表塞 doc。

## ⚠️⚠️ 表结构设计的默认规则：字段展开成列，一对多用关联表

**这是新表的强制规则，不是建议。** 一句话版本，细则见下面的判定问句：

| 情况 | 怎么建 |
| --- | --- |
| **一对多**（先判这个） | **建关联表**，一行一条关系 |
| 字段能列全 | **展开成列**，一个字段一列 |
| 字段集合**真的不可枚举**，或**是一整块没人查的原样载荷** | 才用 `jsonb`，并在文件头写清理由 |

### 判定问句（机械可判，不要凭感觉）

> **你能把这个东西的字段列全吗？**（**按下面的顺序判，先命中先算**）
>
> 1. **是一对多吗？** 是 → **建关联表**，到此为止。
>    ⚠️ **这一条优先于下面全部**，包括「键由用户定」。
> 2. **能列全** → 建列。字段有多少个就建多少列，**没有上界**，「字段多」不是理由。
> 3. **列不全**，因为形状由**别人**决定（第三方回传的原始结构、用户自定义的键值）
>    → 用 jsonb。
> 4. **列不全**，因为**你还没想好** → 想好再建表。这不是 jsonb 的正当用途。

判据落在「**这些键是谁定的**」：由本仓库的 Go 结构体定的，就是可枚举的，建列；
由外部系统或用户定的，才是不可枚举的。

⚠️⚠️ **「用户能自己输入 key」不等于「形状不可枚举」——这是最容易判反的一处。**
用户自定义的环境变量 / 标签 / 偏好项，**形状是固定的**：就是 `key` 与 `value` 两列。
用户定的是**值域**（有哪些 key），不是**形状**。而它同时是一对多，所以照第 1 条走：
**建关联表 + `unique(owner_id, key)`**，先例是 `skill_files` 的
`primary key (skill_id, path)`。jsonb 只留给「**连有几个字段、各是什么类型都说不出**」
的整块载荷。

⚠️ **「逐字节保真」要的是 `text` / `bytea`，不是 jsonb。** jsonb 会重排键、去重复键、
压掉空白、归一化数字——**它恰恰不保真**。本目录就有先例：`run_events.raw` 是权威数据，
那一行的注释原话是「text 而非 jsonb，为的是逐字节保真」。所以第三方回调的原始 body
要留档就存 `text`；只有「要按里面的键查或建索引」时才转 jsonb，而那时你得接受它不保真。

⚠️ **别把「字段集合」读成「取值集合」。** 上面说的可枚举指的是**有哪些字段**。
某一列「现在只有 3 个取值、以后还会加」**根本不影响建表**——它无论如何都是一列
`text`，而本仓库既不用 CHECK 约束也不用 PG enum，加取值是零 DDL。

⚠️ **数组型的「值」算一个标量列，不是一对多。** 向量 embedding、`bytea` 字节、
定长数组存一列即可。「一对多」指的是**能被单独引用、单独更新、需要唯一约束的关系行**
（成员、文件、绑定），判据是「这一项将来会不会被单独查、单独改、或要求不重复」。

**第二个问句（前一问答「能列全」但你仍想用 jsonb 时，必须过这一关）：**

> **有没有任何一个查询，要按这里面的某个字段过滤、排序、分组或去重？**
>
> - **有**（哪怕只有一个）→ **那个字段必须是列**。jsonb 里的键做不到：`->>` 取出来
>   再比用不上索引，唯一约束更是建不了。
> - **一个都没有**，且它是「一整块原样存、原样取、只在人工排查时逐条看」的东西
>   （不定条数的明细、一帧要回放的快照）→ 可以整块进 jsonb。
>
> ⚠️ 这一档**只能装那一块**，不能顺势把整个实体塞进去。判据是「这张表**剩下的**
> 字段是不是都还是真列」——`feedback_reports` 与 `feishu_progress_deliveries`
> 就是这个形状（见下面「正当用途长什么样」）。

⚠️ **「以后可能要加字段」不构成例外。** 加列可以和代码同一版走（见下面「改已有表」
那节——不对称的只有删列）。拿「省一次 alter」换 jsonb，是拿一次五分钟的 DDL 换一个
永久的运行期风险。

⚠️ **「一对多」这条没有例外**，因为它有一个硬理由：**PostgreSQL 无法对 jsonb 数组
元素建唯一约束**。三条路全堵死——`create unique index on t
((jsonb_array_elements_text(doc->'ids')))` 报 `set-returning functions are not allowed
in index expressions`；`exclude using gist` 报 `text[] has no default operator class`
（`btree_gist` 只补标量，不覆盖数组）；生成列报 `cannot use subquery in column
generation expression`。所以关系放 doc 数组里**连去重都做不到**，只能退成「写时在
Go 里查一遍」，那是 check-then-write，多实例并发下必然漏。
先例见 `skill_files.sql` 的文件头。

### ⚠️ 这条规则有机器闸门，不靠自觉

**新增任何 jsonb 列 → `go test ./repository/schema/` 直接红**，报错里带上面那四个
判定问句。判据落在 `.sql` 的**真 DDL** 上（跳过注释），`create table` 与
`alter table ... add column` 两条路都拦。

本仓库承重的约定全都有闸门（漏声明 authz 路由 → 启动 panic；spec 不同步 →
`./openapi/` 红；权限说明漂移 → `./shared/gopkg/authzdoc/` 红），而这条规则如果只写成
文档，它要求的产物是**一段散文**——散文最廉价，写三行注释就能「合规」。加上「往 doc 里
再塞一个键是零 DDL、零报错、当天能合」，那几张 doc 表只会一直长大。

**闸门不阻止合理的例外**：确实属于正当用途就加进 `jsonbguard_test.go` 的
`allowedJSONB` 并写清理由。区别在于那一行会出现在 diff 里——**例外从此是一个要被
解释的决定，而不是一个没人注意到的默认**。

白名单还做**反向对账**：列了但 `.sql` 里已经没有的会红，提醒你拆完 doc 之后把那行删掉。

### 仓库里的现状

- **`workspace_members`** 是关联表而不是 `workspaces.doc.members`：doc 数组给不了
  并发安全（「读整个 doc → 在 Go 里改数组 → 整份写回」，两个管理员同时改成员**后写的
  静默盖掉先写的**）与唯一约束。⚠️ **HTTP 契约照样带完整的 `members` 数组**，读的时候填回去。
- **`issues` / `suggestions` 两个域全是真列，一个 doc 都没有**——而它们**同样进
  HTTP 契约**（`apimodel.Issue` 每个字段都有 json tag，handler 直接 writeJSON）。
  进契约与用列是相容的。
- **52 张表里只有 9 张有 doc 列**（`agents` / `skills` / `runs` / `run_events` /
  `schedules` / `ark_secrets` / `ark_bindings` / `feishu_apps` / `feishu_sessions`），
  **其余 43 张是全列表**。

### 为什么不用 doc：「整体序列化不会漏字段」不成立

`schema.go` 里讲 doc 那段的出发点是「**手写列映射会漏字段，而整体序列化免疫**」。
前半截是真的，后半截不成立，而且是反方向的：

1. **jsonb 并不比列安全，它只是把「漏字段」从编译期挪到运行期且无人发现。**
   改 `ArkBinding.SkillsFingerprint` 的 json tag 加个 `X`，Store 契约套件照样全绿——
   那套用例是 `Put` 完再 `Get`，同一份 tag 序列化又反序列化，**自洽但不锁形状**。
   而已经写进库的行反序列化出空串 → 指纹对不上 → 重建 Environment → 撞 409 →
   **那个空间再也开不了新对话**，全程零报错。
   **列漏一个是编译错误或 42703，doc 漏一个是几周后才发现的数据丢失。**
2. **「抽成列会丢字段」怪的不是列，是没有逐字段往返用例。** 只断言排序、计数、角色的
   用例比不出字段内容；补一条「取值互不相同的逐字段往返」用例就能钉住，而那道防线
   doc 那边**根本没有**（见上一条）。

**doc 的实际代价**：

- **读不了单个字段。** `ListRuns` 只能 `select doc`，把整个空间的 doc 全量取回、在 Go
  里反序列化，开销随行数**线性增长**，而大头在 doc 回传与反序列化，不在 SQL 排序。
- **排序用不上索引的顺序。** `order by coalesce(nullif(doc->>'lastActivityAt',''),
  started_at)` 与 `runs_workspace_started_idx` 的裸列不匹配，必须补一次 Sort。
- **筛选要写 jsonb 表达式且用不上索引。** `runs` 没有 `trigger` 标量列，按触发方式
  筛只能 `doc->>'trigger'`。
- **反范式字段会被永久冻结。** 往 doc 实体上挂人名/头像，那份值跟着 doc 写进库并冻结
  在写入那一刻，那个人改名换头像之后列表里仍是旧的，**且不报任何错**。

### 带 doc 的那几张表怎么办

- **新表必须遵守**，没有「这张表跟 agents 长得像所以照抄」这种理由；
- ⚠️⚠️ **给已有 doc 表加新字段，也要加成列，不要塞进 doc。**
  日常最常见的改动就是「给已有实体加个字段」，而往 `agents.doc` / `runs.doc` /
  `skills.doc` 里再塞一个键是零 DDL、零报错、当天就能合，于是那几张表会**一直长大**，
  规则等于没写。往 doc 里加一个键的代价与新建 doc 表相同：json tag 成了数据格式且没有
  任何测试锁住它（见上面那条 `ArkBinding` 的后果链）。
- 把 doc 里已有的键拆成列时，回填**别包 `coalesce(..., '')`**——那会把「从来没有过
  这个值」写成「有，只是空的」，之后再也分不出哪些行是真的缺。

### 正当用途长什么样

- **`feedback_reports.payload`** —— 二级反馈采的明细（最近几次接口调用、控制台错误），
  形状随前端演进，且**只在人工排查时逐条看**，没有一个查询会按它们过滤。
  而**能查询的几样（`level` / `error_code` / `created_at` / `dedup_key`）全是真列 +
  索引**。
- **`feishu_progress_deliveries.applied_snapshot`** —— 一整帧进度快照，原样存原样取，
  没有任何查询看它内部。同一张表的其余字段全是真列（状态机要按它们认领、重试、排序）。
- **`selfhost_tool_results.result_event`** —— 要原样重发给方舟的事件，形状由 SDK 定，
  只按主键点查。

共同点：**jsonb 只装「没人查」的那部分，而不是装整个实体。**
反过来说——**只要有一个查询要按某个字段过滤 / 排序 / 去重，那个字段就必须是列。**

## ⚠️ 加一张表要改两处

```
1. 在本目录加 <表名>.sql                      （DDL 本身）
2. 在 schema.go 的 coreOrder / authOrder / feishuOrder 里加一行
```

**漏了第 2 步不会编译报错**，只会让表永远不被创建，然后在第一次查询时报
「relation does not exist」——报错指向查询，不指向「你忘了下发脚本」。
反过来（列表里写了、文件没建）**会在启动时 panic**，那是故意的，见 `load` 的注释。
`scripts/gen-schema-outline.py` 也会对账两边，有一边多出来就报错退出。

⚠️ **新表里别放 jsonb 列**（见上面）。真属于正当用途的话还有第 3 步：把那一列加进
`jsonbguard_test.go` 的 `allowedJSONB` 并写清理由，否则 `go test ./repository/schema/` 会红。

## 顺序是显式的，别依赖文件名排序

下发顺序由 `schema.go` 里那三个列表决定。跨文件的依赖只有外键：`workspace_hosts`
引用 `hosts`，所以 `hosts` 在前。

没有用序号前缀（`010_workspaces.sql`）：那与「文件名 = 表名」冲突，而且
`ls` / `filepath.Glob` 的顺序**不保证跨环境一致**——靠它排序等于把下发顺序交给
部署环境决定，那种错在本地必然测不出来。

## 为什么 embed 语句在这里，不在各自的 store_pg.go

**`go:embed` 不能跨父目录**，语言层面的限制，没有开关：

```go
//go:embed ../repository/schema/runs.sql   // invalid pattern syntax，编译失败
```

所以「SQL 文件集中、embed 语句留在原包」这条路**编译期就断**。本目录的
`schema.go` 就是为此存在：embed 跟着 `.sql` 走，各业务包调用
`schema.CorePG()` / `AuthPG()` / `FeishuPG()`。

⚠️ **`all:` 前缀不能省**：不带它时 embed 会**静默跳过**以 `_` 或 `.` 开头的文件。
现在没有这种文件名，但那种漏是零报错的，而加上的代价是零。

目录直接叫 `schema` 而不是 `sql`：三个 `store_pg.go` 都导入了标准库 `database/sql`，
叫 `sql` 就得在每个导入处写别名。

## 三条不能破的约束

**一、脚本每次启动都跑，必须幂等。** 全部是 `create table if not exists` /
`create index if not exists`。

**二、`migrate` 只咽 42P07 / 42710 两个 SQLSTATE。** 那是多实例并发建表的正常结果
（`create table if not exists` 不是原子的，滚动发布期间必然遇到）。别扩大这个范围，
也别改成不咽——不咽的话表现是「随机有实例起不来」，而报错指向建表、不指向并发。
每张表单独一次 Exec，撞上 42P07 只跳过那一张。

**三、列类型的二选一规则。** 进 HTTP 契约、要按字符串排序的用 `text`（不透明 ISO 串，
固定三位小数）；只给数据库比较排序的用 `timestamptz`。
判据是「前端会不会拿这个值做字符串比较」，见 CLAUDE.md 的「时间戳：单位与格式」。

## 改已有表：没有迁移机制，要自己写 alter

现在每份 `.sql` 只有 `create table`，没有任何 `alter`——这是一个全新 schema。
**一旦有了要保留数据的库，改表结构就不能只改 `create table`**：

- ⚠️ **`create table if not exists` 对已存在的表什么都不做，不会补列**；
  `create index if not exists` 只看名字，**不会改已存在索引的谓词**。改了 `create`
  在空库上是对的，在已有的库上静默不生效，而**测试跑的是空库，测不出来**。
- 所以加列要在 `create table` 之后显式写 `alter table ... add column if not exists`；
  改索引谓词要起一个新名字。新列**可空、不给 default**，除非确实需要——
  `not null default ''` 会把已有行伪装成「已经有值了」，之后任何 `where ... is null`
  的回填都是空操作。
- 引用了将来可能被删的列的语句（回填、`alter column`），要包在
  `information_schema.columns` 的存在性判断里：这份脚本每次启动都整份重跑，列删掉之后
  裸写会报 42703，而 42703 在 migrate 里是致命的——**全部实例起不来**。

### ⚠️ 删列必须跨两次发布，不能和「不再读它」同一版

**`drop column` 与「Go 里不再 select 这一列」写进同一个改动，会在滚动发布期间
打出一批 500。** 关键在于**谁先启动谁就先改库**：

```
新实例起来 → 跑 schema → 把 x 列删了
旧实例还在 → 它的 SQL 仍然写着 select ..., x, ...
           → ERROR: column "x" does not exist (SQLSTATE 42703)
```

在新实例改完库、旧实例还没被换掉的那个窗口里，旧实例的每一次查询都失败。如果那张表
在判权的必经之路上（比如 `workspace_members`），受害面就是**全部空间维度接口一起 500**，
而报错完全不指向「有人删了一列」。

⚠️ 别指望「只部一个实例」躲开：滚动发布期间新旧实例必然共存（见 CLAUDE.md
「这是分布式服务」），这个窗口是结构性的。

正确做法是**拆成两次发布**，中间隔一个版本：

| 第几版 | 改什么 | 为什么安全 |
| --- | --- | --- |
| 第 1 版 | 只把 Go 里的读写摘干净（select 列表、scan、insert/update），**DDL 不动** | 列还在，新旧实例都不读它，两边都对 |
| 第 2 版 | 确认第 1 版已全量之后，再加 `drop column if exists` | 此时没有任何在跑的实例会 select 它 |

**加列没有这个问题**（旧实例不认识新列，但它也不 select 它），所以
`add column if not exists` 可以和代码同一版走。**不对称的只有删列。**

## 改完怎么验

编译通过**证明不了 DDL 生效**——embed 断链只会让脚本变成空串，然后在真库上静默
什么都不建。所以起一个 PG 跑一遍：

```bash
docker compose up -d postgres
cd server && go test -race ./repository/...   # 契约用例会在真库的独立 schema 上建表
```

⚠️ **碰库的测试连不上 PG 时整组 `t.Skip`，跳过时是绿的**，用 `-v` 确认 PG 那一支真的跑了。

**动了一批表时，判据是「建出来的库与改动前完全一致」**：在一个干净 schema 上按顺序
跑一遍、把 catalog（列、索引、约束）导出来，改完再跑一遍，两份 diff。
⚠️ **要做变异验证，否则「diff 为空」不算证据**：随手删掉一条索引，diff 必须立刻红。

## 旁边的 dao/

```
repository/dao/
  coredao/     core 的 PG 实现 + 契约套件
  coremem/     core 的 MemStore（AMA_STORE=mem）
  authdao/     auth 的 PG 实现 + 契约套件
  feishudao/   feishu 的 PG 实现（store_pg.go + progress_pg.go）+ 测试
```

- **按子包分而不是平铺**：几份各自都有 `PGStore` / `NewPGStore` / `pgFactory` /
  `memFactory` / `storeFactory` / `initPGEnv` / `testSchema` / `sqlState*` / `schemaSQL`，
  平铺会撞名。分子包之后每份保留天然名字，且测试二进制各自持有 `sync.Once`，
  不共享 PG 测试环境。
- **契约套件跟着实现放在 dao 里**，否则 `core(test) -> dao -> core` 成环。
  ⚠️ `go build` 证明不了没有环（环只在测试里），只有 `go vet` 才报。
  ⚠️ `export_test.go` 那条路走不通：它只在**自己包**的测试二进制里编译，对
  `repository/dao/*` 根本不可见。同理**共享测试夹具包也不成立**
  （`feishu(test) -> 夹具包 -> feishu` 就是环），所以 `feishudao/fakes_test.go`
  是**刻意复制**的，改动要同步两边。
- **未导出依赖按「它属于哪一层」处理**，而不是一律导出：
  - 两个实现共有的不变量 → 下沉叶子包：`persistmodel.LastActivityOf` /
    `MaskSecret` / `NormalizeEmail`，以及 `feishu/feishumodel` 那几个落库状态串与
    `SessionTTL`。**各写一遍迟早分叉，而分叉不报错**，只表现为「按新值查一条都查不到」。
  - PG 专用 → 跟着实现放：`snapshotJSON` / `snapshotFromJSON`。
  - 只有测试要 → 在 dao 侧本地写：`authdao` 的 `testKeyHash`（不为测试导出生产的
    `hashAPIKey`）。
  - 要给测试开口子 → 给**构造函数或带 `ForTest` 后缀的访问器**，不要导出字段：
    `feishu.NewMemStoreWithClock`（字段导出后能建出半初始化的 memStore，
    漏一个 map 就在第一次写入时 panic）。
- **接口留在业务包**——接口在消费方声明是依赖倒置要求的方向，把它挪进
  `repository/dao` 反而是退回传统三层。
- **`coremem` 一个 `core` 符号都不能用**——`core` 自己的内部测试（`package core`）要构造
  MemStore，加一行 `import core` 就成 `core -> coremem -> core` 的测试环。它只能依赖叶子包
  （`model/apimodel` / `model/persistmodel`）。隔壁 `coredao` 能 import `core`，是因为没有
  任何 `package core` 的测试构造 PGStore——**能不能 import 业务包取决于「谁在测试里构造它」**。
- **没有单独的 PO 层**：字段本来就展开成列，行结构体直接落在 `model/persistmodel`，
  再插一层只是多一次同义翻译。
