# 前端

**读它的时机**：改 `web/src` 下任何代码之前。

动手前先读 **`docs/DESIGN.md`**——路由结构、数据模型的命名约定、33 类事件的呈现策略
都在那里定好了。配色与圆角见 [styling.md](styling.md)，文案见 [i18n.md](i18n.md)，
组件坑见 [heroui.md](heroui.md)。

## 现状

功能已完整落地（workspace / agent / 对话 / 技能 / 飞书 / 运行历史 / 空间设置），
`npm run typecheck`、`npm run build`、`npm run e2e`（116 个用例）均通过。

## 数据层

页面只依赖 `src/data/api.ts` 里的 `ApiClient` 接口，有两份实现：

| 实现 | 位置 | 何时生效 |
| --- | --- | --- |
| `HttpApiClient` | `src/data/http.ts` | **默认**，打 `server/` 的 Go 服务 |
| `LocalApiClient` | `src/data/api.ts` | `VITE_API_MODE=local` 时 |

接口前缀默认是同源的 `/api`，由 `vite.config.ts` 的 `server.proxy` 转给
`127.0.0.1:8080`——**同源就没有跨域**，不用后端配 `AMA_ALLOWED_ORIGIN`。
后端换端口用 `AMA_PROXY_TARGET` 覆盖代理目标，要连完全另一个地址则设
`VITE_API_BASE`（那条路得后端自己允许跨域）。三个环境变量都声明在 `src/vite-env.d.ts`。

所以本地开发要**先起后端**：

```bash
cd server && AMA_DEV_ROUTES=1 go run ./cmd/amad
```

没起后端时页面会显示「连不上服务端，请确认后端已启动」——这是 `http.ts` 把 fetch 的
`Failed to fetch` 换成的中文，看到它就是这个原因。只想调 UI 可以
`VITE_API_MODE=local npm run dev` 回到 mock。

**`LocalApiClient` 仍是后端契约的真源**——它的每条副作用、每句报错文案、每个字段的
有无都被 `server/core` 照抄。改它等于改后端契约，见 [backend.md](backend.md)。
它同时是 e2e 的运行底座，见下。

`window.__amaApi` 暴露了 `reset()` / `clear()`（HTTP 那份打后端的 `_debug` 路由，
只在 `AMA_DEV_ROUTES=1` 时存在）。另外两个 **只有 LocalApiClient 有**：

- `completeFeishuAuth()`——真实的设备码授权只能由用户在飞书点同意，后端不该有一个能
  凭空造出授权结果的接口。
- `flushConversation()`——把对话的模拟回放一次性推到底。逐字出字对用户是功能，对 e2e
  是不确定性；有它之后用例既能断言中间态（不调）也能断言最终结果（调），不必 sleep
  等动画。
- `signOut()` / `completeLogin()`——把 mock 切成未登录、以及模拟「用户在 SSO 页点了同意」。
  同 `completeFeishuAuth` 的理由，只为让未登录态可测（见下面「未登录态」一节）。

### 三条写 HttpApiClient 时踩过的坑

**可空 getter 认 `code` 不认状态码。** `getWorkspaceByKey` / `getAgent` / `getSkill` /
`getRun` / `getFeishuApp` 要把「不存在」转成 `null`，但判据必须是错误信封里的
`not_found` / `app_not_found`。按状态码判会把「路由挂错了」（后端回
`route_not_found`）也变成 `null`，页面上表现为**静默的空状态**，从现象根本查不到原因。

**`getArkCredential` 永远不为 null。** 未配置时后端回一个空掩码的空壳。页面靠
`cred === null` 判「加载中」、靠空掩码判「未配置」，多加一层 null 兜底会让读失败
永远停在骨架屏。同理 `listRunEvents` 对不存在的 run 回 `200 []` 而不是 404。

**DELETE 回 204 且没有 body。** 无条件 `res.json()` 会在空 body 上抛异常，把每次
成功的删除都变成错误弹窗。

### e2e 跑在 local 模式下

`playwright.config.ts` 给 webServer 设了 `VITE_API_MODE=local`，并用**独立端口 5274**、
`reuseExistingServer: false`。两件事都是必须的：116 个用例依赖 mock 的确定性（每个用例
前 `reset()` 回种子数据、飞书那几条要 `completeFeishuAuth()` 跨过授权），而复用 5273
上日常开发的 server 会静默地让用例跑在 HTTP 模式的页面上，报错完全不指向真实原因。

**跑整套时 dev server 会中途死掉**，之后所有用例以 `ERR_CONNECTION_REFUSED` 连片失败
——**看着像大面积回归，其实是环境**。判据：错误停在 `fixtures.ts` 的 `page.goto`，
而单独跑那个 spec 又全绿。每次死在哪个用例还不固定，所以别照着失败列表找代码问题。

根因是 `fs.inotify.max_user_instances` 只有 128 且常被别的进程占到 124（`vite.config.ts`
已经配了 `usePolling` 但仍不够）。**要一次跑完全部用例，用 preview 而不是 dev server**
——静态产物、零文件监听：

```bash
VITE_API_MODE=local npx vite build --mode development
npx vite preview --port 5274 --host 0.0.0.0 --strictPort &
# 再拿一份 baseURL 指向 5274、不带 webServer 的 config 跑 playwright
```

实测这样一轮能跑完全部用例。顺带一个坑：**那份临时 config 必须放在 `web/` 里**，
放 `/tmp` 会 `ERR_MODULE_NOT_FOUND`（解析不到 `@playwright/test`）。

另外 5274 上的 server 常有残留（Playwright 中断后没收干净），下次跑报
「port is already used」。⚠️ **先别急着 kill——这个工作区随时有十几个会话在干活，
占着 5274 的多半是别人正在跑的测试，不是你的残留。**
默认做法是换自己的端口：`E2E_PORT=<没人用的> npx playwright test`
（配合下面「别用共享的 `web/dist/` 当 e2e 底座」里的私有 outDir 一起用）。
只有 `ss -ltnp | grep 5274` 拿到的 pid **确认是你自己起的**（比对启动时间与 cwd）
才 `kill -9`。

#### ⚠️ 别用共享的 `web/dist/` 当 e2e 底座

`dist/` 是**整个工作区共用的一份**，而这里随时有十几个会话在干活。你起了 preview
指着它，别人一句不带 `VITE_API_MODE=local` 的 `npm run build` 就把你脚下的产物换成
HTTP 模式的——**preview 进程不用重启就开始服务对方的构建**。

那样产物里没有 `LocalApiClient`，`app` fixture 的 `reset()` 写不进 mock store
（`save()` 那层还会静默吞掉异常），`AuthGate` 判匿名，于是**每条用例都停在落地页**。
失败截图是登录页，于是所有解释都往「鉴权被我改坏了」上跑——实测这个假象连着骗过
三次归因（先怀疑 `12-landing` 的 signOut 泄漏、再怀疑 seed 数据、再怀疑 localStorage
配额）。

判据只有一句，不成立就别往下查：

```bash
grep -c LocalApiClient web/dist/assets/index-*.js   # 0 = 不是 local 构建
```

正确做法是把产物构建到**自己的 outDir**，从此与别人无关：

```bash
# ⚠️ 构建要用真 config 加 --outDir。用 `--config 自己那份` 会整份替换掉
# vite.config.ts，`@` 别名和插件全丢，报 Rollup failed to resolve import "@/router"。
VITE_API_MODE=local npx vite build --mode development \
  --outDir /tmp/dist-<你的名字> --emptyOutDir

# preview 只读 build.outDir，所以这一步才用得上那份两行的临时 config
npx vite preview --config vite.<你的名字>.config.ts --port <你的端口> --strictPort
```

端口也要挑没人用的（`ss -ltn | grep -oE ':52[0-9][0-9]'` 先看一眼），
**别 kill 占着 5274/5279 的进程**——那多半是别人正在跑的测试。

## 技能详情页：SKILL.md 是虚拟节点

`/w/:key/skills/:skillId`（`pages/SkillDetailPage.tsx`）列出一个技能里的所有文件。
**先记住这条，别做歪**：

```
skill.instructions  ← SKILL.md 的正文，唯一真源
skill.files[]       ← 除 SKILL.md 以外的附件，只有 path + size
```

后端解析技能包时把 SKILL.md 的正文抽进 `instructions`，**刻意不重复存进 `files`**
（`server/service/skill/skillpkg.go`：「同一份内容有两个真源，改了正文附件里还是旧的」）。
所以文件列表里那条 `SKILL.md` 是**页面拼出来的虚拟节点**，内容来自 `instructions`。

这个区分决定了「保存」往哪走：**正文走 `updateSkill`（改 instructions）、附件走
`updateSkillFile`**（详见下面「编辑」一节）。做成「两个地方都能编辑同一份内容」
就会互相打脸。虚拟节点的 key 用
`" body"` 而不是字面量 `"SKILL.md"`——万一附件里真出现同名文件，两者的 key 会撞在
一起，表现是点其中一个高亮另一个。

三条：

- **附件内容要单独取**（`readSkillFile(skillId, path)`）。列表与 `getSkill` 的响应里
  `files[]` 只有 path 与 size，后端刻意省掉字节（否则响应随附件线性膨胀）。
- **`SkillFileContent.content` 是明文，不是 base64**，而 `SkillFile.content` 是
  base64（Go 侧 `[]byte`）。同一个概念两种编码，对前者再 decode 一遍会得到乱码。
- **二进制不渲染内容**（`binary: true`），但要说清「它仍然会被写进容器」，否则用户
  以为这个文件坏了。截断同理要明说。三态分开渲染，照 `SandboxPanel` 那套。

### 文件列表按目录折成树，不平铺完整路径

`lib/skillFileTree.ts` 的 `buildSkillFileTree()` 把扁平路径清单折成目录树。
理由是信息密度：一个技能有 7 个 `references/*.md` 时，那个前缀在每一行重复、
占掉半行宽度却不携带任何区分信息。

```
平铺（改之前）                      折成树（现在）
references/anywheredoor.md          references/
references/client.md                  anywheredoor.md   9 KiB
references/component.md               client.md        16 KiB
```

三条不能做错的地方：

- **必须递归，不能只按第一个 `/` 切一刀。** 后端对路径深度没有限制
  （`scripts/foo/bar.sh` 就是它注释里的例子），只折一层的话 `a/b/c.md` 会显示成
  一个叫 `b/c.md` 的文件挂在 `a` 下面——**不报错，只是不对**。这类「后端支持 N 级、
  前端只接一级」的缺口零报错，由 `29-skill-detail` 那条「多层目录逐级折进去」钉住
  （做过变异验证：改成只折一层就红）。
- **显示名与接口入参是两个东西。** 行内显示叶子名（`name`），而
  `readSkillFile` 的入参与 `data-path` 必须是**完整相对路径**。混用的表现是
  点开二级目录里的文件时后端回「技能里没有这个文件」。
- **缩进用内联 `style` 不要拼 `pl-${n}` 类名。** Tailwind 静态扫描源码，拼出来的
  类名它扫不到、不产出任何 CSS，于是缩进完全不生效**且不报错**（同下面
  「写不存在的 token，Tailwind 静默不产出」那条）。

目录**只分组、不可点**（没有内容可看，做成按钮会让人点了发现右侧没反应），
也**不做折叠**——附件上限 64 条、真实技能包通常不到 10 条，全展开一眼看完比多一层
交互好。

**列表页的入口只把标题做成 `<Link>`，没有整卡包链接**——技能卡片右上角有 2~3 个按钮，
外面再套一个链接就是嵌套可交互元素（`AgentsPage` 能整卡包是因为它卡里没有按钮）。

### 内置技能在 local 模式下正文是空的

`data/builtinSkills.ts` 刻意不抄正文（那两段有几十行、跟着后端改，抄进来必然漂移）。
代价是**详情页在 mock 下看到的是空正文**，所以页面对空 `instructions` 显示一句
「正文由平台维护」而不是留白——留白看着像页面坏了。

### e2e 要造附件只能用钩子

种子里 4 条技能**一条都没有 `files`**，而且加不了：Go 侧 `TestSeedRoundTrip` 拿
`seed.json` **逐字节**比对，多一个 `"files":[]` 就红。加上 mock 刻意丢 content、
又不解 zip，于是带附件的场景只能靠 `seedSkillFilesMock(page, skillId, files)`
（`e2e/fixtures.ts`）。

内容落在**独立的 localStorage key** 上（`reset()` 冲不掉，否则用例前的 reset 会把它清了），
所以**用例必须在 `afterEach` 调 `clearSkillFilesMock`**——同 `failReadsMock` 那批的约定。
造二进制文件：内容里放一个 `"\u0000"`（判据与后端 `core.isBinary` 一致）。
⚠️ 写**转义序列**而不是字面的 NUL 字节：后者在源码里不可见，格式化与复制粘贴都可能
把它吃掉，而吃掉之后那条分支永远为假、用例静默走进文本分支。

### 编辑：两条保存路径，一个界面

```
SKILL.md → api.updateSkill({instructions})   改正文（PATCH /api/skills/{id}）
附件     → api.updateSkillFile(id, path, s)  改附件（PUT /api/skills/{id}/file）
```

**是两条完全不同的接口，但对用户是同一件事**「改这个文件然后保存」。所以交互必须长得
一样——同一个位置的保存按钮、同一套脏状态、同一句保存后的提示。差异被关在
`FileEditor` 的 `onSave` 一个 prop 里，调用方各传自己那条。做成「两个地方都能编辑同一
份内容」就会互相打脸（那也是为什么后端拒收 `path=SKILL.md`）。

五条：

- **保存按钮只作用于当前选中的那个文件，且文案带文件名**（`保存 scripts/init.sh`）。
  不带名字的话，用户在几个文件间切来切去、改了两个只存了一个，界面上没有任何东西
  能告诉他刚才存的是哪一份。
- **切文件时有未保存改动要拦一下**，并给「放弃改动」这条出路。静默丢掉用户改的半屏
  内容是这个交互最坏的失败方式。草稿**按文件存**（`Record<path, string|null>`），
  不是一个全局草稿——否则切走那一下就丢了。
- **二进制与被截断的文件都不给编辑。** 拿 textarea 编辑二进制会直接毁掉那个文件
  （它只能装合法 UTF-16 文本）；而保存一份只有前 256 KiB 的内容，等于把后面那截
  静默删掉。
- **保存后必须说「新会话才生效」**（`skill.savedNeedsNewSession`）。技能文件是
  **建容器那一刻**注入的，正在跑的会话里那份不会更新，而 agent 读到旧内容
  **不报任何错**。不说这句用户会以为没存上、然后反复点保存。
- **保存成功用服务端返回的对象覆盖本地 state，不走 `reload()`。** reload 会把
  `AttachmentView` 整个重挂、内容要重新取一次，而我们手里已经有完整的新对象了。
  顺带这让「保存后 `files[].size` 立刻更新」不需要多一次往返。

⚠️ **`SkillFileContent.content` 是明文不是 base64**（后端 Go 侧就是 `string`），
而 `SkillFile.content` 是 base64（`[]byte`）。同一个概念两种编码，对前者再 decode
一遍会得到乱码。

### 只读技能：两处闸门，前端这道只是体验

`isReadOnlySkill`（内置技能 builtin）为真时**整个不渲染编辑入口**，
只给一条可点的「先复制一份（Fork）」。后端 `Service.UpdateSkillFile` 里还有一道
（它绕开了 `UpdateSkill` 的检查，所以自己重写了一遍）——那道才是安全边界，
前端这道是为了别让用户点了才知道不行。

⚠️ **`isReadOnlySkill` 是 `===` 比较而不是 exhaustive switch**，所以加来源时漏了它
**不会有编译错误**：新来源静默变成「可编辑」，技能库渲染出 Edit 按钮、详情页正文与
附件都能编辑，而用户只在**点保存那一刻**才撞上后端的 403。

⚠️ 来源徽章有**四处各自独立的渲染点**：技能库卡片（`SkillsPage`）、技能详情页
（`SkillDetailPage`）、Agent 详情页的绑定列表（`AgentDetailPage`）、建 Agent 时的
技能勾选区（`AgentsPage`）。判据收在 `skillSourceBadge`（`types/domain.ts`），
但加一个来源时四处都要看——加一处漏三处不报错，只是那个技能在某些页面上看起来像自建的。

## 对话页

**一次对话就是一条 `trigger: "manual"` 的 `Run`，消息就是 `RunEvent`**——没有独立的
Conversation 实体。所以对话自动进运行历史、能从历史点回来续聊、工具调用的配对直接复用
`lib/timeline.ts` 的 `buildTimeline()`。理由与两种呈现的差异见 `DESIGN.md` 3.6.3。

### 呈现方式在 `?view=`，默认对话视图

运行详情页与对话页都有「对话 / 事件」两种呈现，共用 `components/ViewSwitch.tsx`
（`useRunView()` 读写 URL）。**缺省是对话视图**，只有 `?view=events` 才是那条 33 类
事件的竖向时间线。放 URL 而不是组件状态，是为了链接可分享、刷新不丢（同
Agent 详情页的 `?tab=`）。

由此有两条：

- **写 e2e 要断言时间线时，直接 `goto` 带 `?view=events` 的地址**，别去点开关 ——
  确定性更高、不用等动画。整套里凡是断言 `timeline` / `event-*` / `filter-*` /
  `thread-*` 的用例都得带上它，漏了就是「元素找不到」而报错不指向真实原因。
- 按轮折叠的逻辑在 `lib/timeline.ts` 的 `groupTurns()`（纯函数，两个页面共用），
  渲染在 `components/ConversationView.tsx`。**`ConversationView` 刻意不含
  `ChatConversation` 滚动壳**：运行详情页是页面流滚动，套上那个
  `flex:1; overflow-y:auto` 的容器会得到两层滚动区。对话页那层壳由 `ChatStream` 提供。

### Trace 视图的筛选轴按方舟的真实分类，不自造分桶

事件视图（`?view=events`）的筛选建在**三条真实存在**的轴上，控件在
`components/TimelineToolbar.tsx`，状态在 `lib/useTimelineFilter.ts`：

| 轴 | 值域 | 出处 |
| --- | --- | --- |
| 域 | 4：`user` / `agent` / `session` / `span` | 方舟 `{domain}.{action}` 约定 |
| 语义类别 | 8 个 `EventCategory` | AMA 自己的呈现分组，DESIGN.md 3.6.2 |
| 具体类型 | 34，只列本次运行出现过的 | `RunEventType` |

「Tool / Model / Error / System」这类口语诉求做成**预设**（`FACET_PRESETS`），一键落到
上面两条轴上，而不是新造一套枚举 —— 域与类别是**交叉**关系：一次工具调用横跨 user 与
agent 两域，`session.status_*` 与 `session.thread_status_*` 同属 session 域却分属两个类别。

四个坑，每个都不报错：

- **`applyPreset("model")` 必须同时设类别。** `span.model_request_*` 属类别 `trace`，
  而 trace 默认隐藏。只设 `types` 不设 `categories`，点了「模型」一条都不显示。
- **写 URL 参数的基准必须是 `window.location.search`。** 连着改两处筛选（点预设、
  紧接着敲关键词）时，闭包里的 `params` 是上一次渲染的快照，第一次的改动会被静默覆盖。
  **`setSearchParams` 的函数式重载不解决这件事** —— react-router 7 把它实现成
  `nextInit(new URLSearchParams(searchParams))`，参数仍是那份按 `[location.search]`
  memo 的快照。地址栏是唯一 always-current 的真源。
- **不要新增以 `event-` 开头的 testid。** e2e 用 `[data-testid^="event-"]` 数时间线条目，
  任何 `event-*` 都会被算成一条事件（那次加了 `event-duration`，表现是「按线程筛完条目数
  没变」）。行内的辅助标记用别的前缀，如 `row-duration`。
- **断言条目数要用会重试的 `toHaveCount` / `expect.poll`。** 筛选态在 URL 里，点一下要
  多走一个路由 tick，裸 `count()` 不重试，会读到上一帧。

关键词搜索覆盖工具入参、stdout/stderr、返回值与**原始 payload JSON**。原始 payload 要
`JSON.stringify`，所以 haystack 必须 memo 化（`buildHaystack`，依赖 `[items]`），
否则每敲一个字都把全部条目重新序列化一遍。

### 退出码 / stdout / stderr 是从结果文本里解析出来的

后端**没有**这些结构化字段。`agent.tool_result` 的载荷只有 `content` 加一个 `is_error`；
真正的执行细节被 `server/shared/infra/shelltools/bash.go` 的 `formatResult()` 拼成一段文本：

```
exit_code: 0                    ← 总是第一行；未退出时是字面中文 (未退出)
--- sandbox error ---           ← 可选，容器没起来（与进程 stderr 是两回事）
--- stdout ---
--- stderr ---
…（输出过长，已截断，共 N 字节）  ← 超 64 KiB
```

`lib/toolResult.ts` 按这个格式解析，**解析不出就整段原样显示**（MCP、read/write/grep 与
种子数据都走这条退化路径）。三条：

- **这是与 Go 侧的隐式约定**，改 `formatResult()` 会静默让前端退回整段显示。两边都留了
  注释指向对方。
- **不能 `split("---")`** —— stdout 里出现 `---` 太正常（markdown 分隔线、`--- a/file`），
  必须行锚定。判据是 `exit_code:` 行匹配成功，不是「有任一段标记」。
- **退出码 ≠ 成败。** `grep` 无匹配退出 1，`shelltools` 据此把 `is_error` 置 true。所以成败
  仍以事件的 `is_error` 为准，退出码是**中性 Chip**（非 0 用 warning 不用 danger）。

### 耗时是派生的墙钟时间，不是执行时间

方舟不给任何 duration 字段，只有 `processed_at`（秒级）。工具耗时由「回执的
`processed_at` − 调用的」算出（`toolCallItem`），任一端为 `null` 就是 `undefined` ——
**不能当 0**，那会让它在耗时排序里冒到最快那一头。

**这个值含排队，也含会话第一次调工具时建容器的时间**（没拉过镜像时还要拉镜像，见
`shared/infra/dockertools`）。所以
「> 5s」的语义是「这一步让用户等了 5 秒以上」，用它找「卡在哪」对，用它论证「命令本身慢」
不对。UI 上那句说明（`filter.slowOnlyHint` / `timeline.durationHint`）是功能的一部分，
不是装饰。

**折叠态必须受控（`isExpanded` + `onExpandedChange`），不能用 `defaultExpanded`。**
后者是非受控属性、只在挂载时读一次，而一轮对话的真实过程是「跑的时候摊开 → 收尾后折叠」,
组件早就挂载过了，改 `defaultExpanded` **一点作用都没有**：表现是回答已经出来、
这一轮结束了，中间那堆工具还赖着不收，正好把「turn 结束只留输入输出」这条需求废掉。
typecheck 与 build 都不会报，只有 e2e 断言「结束后工具行不可见」才抓得到。

实时性走方舟 SSE 的 `event_deltas` opt-in，逐字出字。三个要点：

1. **增量帧不在 `RunEvent` 联合里**，是单独的 `StreamFrame`：方舟明确它 best-effort 且
   不进 event history，混进去会让所有按 `RunEvent` 穷举的地方都要处理一种查不到的东西。
2. **先补历史再订阅**（`useConversationStream.ts`）。方舟不回放，反了就丢中间那段。
3. 草稿只是显示脚手架，**最终事件到达时按 id 整体替换**——这样丢帧也不影响最终结果。

`HttpApiClient` 那侧用原生 `EventSource`：断线重连与忽略 `:` 心跳注释行浏览器已经做了。
代价是它不能带自定义头，所以身份只能进 query——**接真实鉴权时这里要一起改**。

### 连着发消息会排队，agent 答完自动发出

**这是排队功能的主路径**，下面那条 409 是兜底。用户连着说几句话时，第二句起会停在
输入框上方排队（`lib/useSendQueue.ts` + `components/SendQueueList.tsx`），
这一轮结束后按先进先出自动发出。每条可单独取消、可点「修改」把文案取回输入框。

原先这条路是**堵死的**，而且是四个独立的坑叠在一起，每一个都零报错：

1. **`ChatComposer` 的 `canSend` 带着 `!streaming`**，且「发送」按钮在 streaming 时
   整个被「中断」替换掉 —— 于是 agent 一开始答，用户就没有「发送」可点。
   现在两个按钮**并存**（中断是 icon-only + Tooltip，发送仍是主 CTA）：
   停下这一轮和再说一句是两件不冲突的事，互斥没有道理。
2. **`useConversationStream` 的 effect 会把 `streaming` 抹回 false。** 新建会话是
   「建 run → `markStreaming()` → navigate 换 URL → 发消息」，换 URL 让 effect 以新
   runId 重跑，无条件 `{...INITIAL}` 正好抹掉刚置上的标记。于是 `send()` 读到
   `streaming === false` 就**直发**，绕过队列。现在用 `markedRef`（带 run 归属，
   切会话时清）承载这条信息 —— 它必须是 ref，因为 state 会被同一个 effect 重置。
3. **`markStreaming()` 让「中断」按钮比消息落地更早可点。** 于是中断能跑在
   `sendMessage` 之前，`user.interrupt` 落在 `user.message` 前面 —— 那一轮看起来
   「先被中断、然后才开始」，而被中断的那句话照常跑完。现在 `interrupt()` 先
   `await` 在飞的那一发（`inflightSend` ref）。
4. **`ConversationView` 的 `isSubstantive` 把只剩「用户中断」的段整段丢掉。**
   `user.interrupt` 的 category 是 `status`，落进 `default: false`。用户在 agent
   还没说话时就按停，那一轮**什么都不显示**——发出去的问题后面一片空白，看不出
   是自己中断的。现在 `status` 分支单独放行 `user.interrupt`。
   这与 `LIFECYCLE_CHURN` 是同一条纪律的两半：那个管段内过滤，这个管整段存亡。

⚠️ **第 4 条早先测不出来**，是因为 e2e 总在思考/工具已经出来之后才点中断，段里还有
别的实质内容顶着。它是被第 1 条（两个按钮并存 → 中断能点得更早）暴露出来的。

⚠️ **`09-chat` 那条「中断正在跑的对话」原先靠「发送替换中断」当隐式同步点。**
两个按钮并存之后 `expect(chat-send).toBeVisible()` 瞬间就满足，后面找「用户中断」
会跑在事件到达之前。判据要改用「中断按钮消失」——那才真表示 streaming 落下来了。

队列的行为约束：

- **`streaming` 期间队列完全静止**（不排定时器、不轮询），靠 `ChatPage` 在 streaming
  落下来时调 `queue.resume(runId)` 驱动。漏了那个 effect，消息会永远停在队列里。
- **队列项要记住自己的 `runId`**，否则用户在排队期间切走，话会发进别人的会话。
- **队列非空时新消息一律排队尾**，不许抢先直发（否则两句话会调序）。
- **上限 20，满了交回原文 + 给提示**；排到 3 分钟放弃，同样交回原文。静默丢弃是
  这个功能最坏的失败方式 —— 用户以为发出去了，然后等一条永远不会来的回复。

mock 侧有两个开关，**必须分开**：`holdTurnMock()` 让这一轮停在「进行中」
（`streaming` 为真在 e2e 里既等不到也造不出来：回放 1.4 秒就跑完，而六条种子 run 的
事件尾部全是 `status_idle`，连 `status: "running"` 的 run_4 也是），
`conflictSendsMock(n)` 造 409。两者语义相反，见 `e2e/fixtures.ts` 里的说明。

### 撞上工具执行窗口的消息会排队重发，不报错

self_hosted 下方舟发出 `agent.tool_use` 到 worker 回传 `user.tool_result` 之间有个几十
毫秒的窗口（线上实测 48ms），窗口内方舟拒收 `user.message`，后端翻成 **409 +
`code=conflict`**（`server/core/chat.go` 的 `arkFailure`）。

**它是常态不是故障，所以前端替用户排队重发**（`lib/useSendQueue.ts` +
`components/SendQueueList.tsx`）。用户为什么会频繁撞上：每次工具调用都会推一条
`session.status_idle`，前端据此把 `streaming` 落下来、按钮从「中断」变回「发送」——
**整个工具执行期间界面看着是空闲的**，一发就撞窗口。

五条，每条都不报错：

- **判据是 `e.code === "conflict"`，不是 HTTP 409、更不是匹配那句中文。** 按状态码判会
  把将来别的 409 语义（「技能名已存在」之类）也拖进自动重发，用户对着一条永远发不出去的
  消息干等三分钟；按文案判则改文案就静默失效。这条由 `27-send-queue.spec.ts` 里那条
  「500 + code=conflict 同样走排队」钉住（`failWritesMock("conflict")` 造的正是
  code 对、状态码不对的形状，**那是分辨两种实现的唯一手段**）。
- **发送失败必须撤回 `markStreaming()`**（`useConversationStream` 的 `clearStreaming`）。
  那个标记是乐观的，而这一轮压根没开始，没有任何事件会来把它落下去——不撤的话
  `streaming` 永久为 true，输入区停在「中断」态，**用户在排队期间一条都发不出去**。
- **队列项要记住自己的 `runId`。** 拿「当前 URL 上的 runId」重发的话，用户在排队期间切去
  另一条会话，这句话就发进别人的对话里了。
- **队列非空时新消息一律排队尾，不许抢先直发**，否则窗口恰好在这时关闭会让两句话调序。
- **退避有上限、到点放弃**（1.8 倍退避封顶 5s、总时限 3 分钟），并把原文交回输入框。
  队列满 20 条同样交回原文 + 给提示——**静默丢弃是这个功能最坏的失败方式**，用户以为
  发出去了，然后等一条永远不会来的回复。

「排到超时」与「真故障」要分开：前者走中性的 `Notice`，**不能**走 `sendFail`——那会染成
「发送失败」的红字并上报一条假故障，污染错误率与反馈库。

mock 侧的开关是 `conflictSendsMock(n)`（撞 n 次后自然成功，`-1` 一直撞）。**它与
`failWritesMock` 必须分开**：那个造永久失败（该摊给用户），这个造几十毫秒的竞态
（该悄悄重发）。合成一个的话「重发能成功」这半条路根本测不到——永远失败的 mock 下
队列只会一直重试到超时，那是放弃分支不是成功分支。

### agent 被删掉的会话是冻结的

判据只用后端算好的 `Run.agentDeleted`，**别拿本地 `agents` 列表反推**——双源判据会在
「列表还没加载完」的首帧闪一下「已删除」。冻结时输入框、agent 选择器、发送按钮全禁用
（`ChatComposer` 的 `isDisabled` 一条线全打通了），但**中断按钮保持可点**，与后端放行
`user.interrupt` 一致。

`LocalApiClient` 那侧的 `sendMessage` 也有同一道闸门，**报错文案必须与后端逐字一致**
（它是契约真源，e2e 又直接断言那句中文）。`interruptRun` 刻意不加。

新的行内告警条一律用 `components/Page.tsx` 的 `Notice`；「已删除」标记用同文件的
`AgentDeletedChip`。那个告警条形状在仓库里被手抄过六处（`FeishuReadiness` 的私有
`Issue`、`SettingsPage` 等），**待收敛**，本次没顺手改是因为那是跨六文件的纯 churn。

## 方舟事件的实际形状与文档不一致

方舟 Managed Agents 的官方文档**它在这三处与真方舟下发的不符**。
三条都是「mock 下永远碰不到、接真方舟必现」，且都不会编译报错：

| 字段 | 文档说 | 实测真方舟 | 照文档写会怎样 |
| --- | --- | --- | --- |
| `agent.tool_use.input` | `string`（JSON 格式字符串） | **对象** | `truncate` 里 `.replace()` 抛 TypeError → **整页白屏** |
| `agent.tool_use.tool_use_id` | 必有 | **根本不下发**，调用 id 就是事件的 `id` | 配对键全是 `undefined`，一轮里多次调用互相覆盖 |
| SSE 增量帧 | 未明确形状 | `{kind: "event_start" \| "event_delta", id, text}` | （这条前端猜对了，parseFrame 不用改） |

所以：

- **入参一律走 `prettyJson()` / `truncate()`**，它们接 `unknown` 且两种形状都吃。
  不要直接 `.replace()` 或 `JSON.parse()`。
- **配对键一律写 `tool_use_id ?? id`**，且 `indexToolCalls` 与 `buildTimeline` 两处
  取键方式必须一致（不一致就查不到那条 call，工具行整条消失）。
- 类型声明按**实测**写（`ToolInput`、`tool_use_id?`），不要按文档写——
  按 `string` 声明才是根因，它让调用方理直气壮地 `.replace()`。

重复 key 的警告值得当 bug 查：`Encountered two children with the same key` 那次
就是 `tool_use_id` 缺失导致 3 条工具调用塌成 1 个键，症状是控制台警告，
真实后果是工具行少两条、结果永远配不上。

## 裸 await + 骨架屏 = 白屏，且零报错

**加载数据一律用 `lib/useAsyncData.ts`，不要自己写 `load` + `useEffect`。**

这条是踩出来的。原先 11 个页面各自写了一遍这个形状：

```ts
const load = useCallback(async () => {
  setData(await api.listXxx(workspace.id));   // 裸 await，没有 try/catch
}, [workspace.id]);
useEffect(() => { void load(); }, [load]);
if (data === null) return <ListSkeleton />;   // 加载态 = data 还是 null
```

它有一个**零报错**的故障：`await` 一旦 reject，`setData` 永不执行，`data` 永远是
`null`，页面**永久停在骨架屏**——用户看到的就是白屏，界面上没有任何提示，
只有控制台里有一条 unhandled rejection，而 `void load()` 连那条的显式性都吞掉了。
`Promise.all` 让它更容易触发：**四个接口里任何一个** reject 就是这个结果。

**为什么直到 RBAC 上线才暴露**：mock 与本地开发下这些接口从不失败。`c301fe8` 之后
非成员/角色不够一律 403，403 正好走这条没有 catch 的路径。触发它不需要「被踢出空间」
这种罕见情况——**网关与接口的判据不一样**：`GetWorkspaceByKey` 只判「在不在成员表里」
（`memberOf`），而那四个接口判「角色够不够 viewer」（`levelOf(role) >= LevelViewer`）。
角色是个字符串，落库的值不受枚举约束，所以「在成员表里、角色不在四个枚举里」的人
**进得了空间、四个接口全 403**，正好撞在这条路径上。

所以：

- **加载态用 `isLoading`，不要用 `data === null`**——失败时 `data` 也是 `null`，
  那正是永久骨架屏的成因。
- **错误态用 `components/Page.tsx` 的 `AsyncErrorBlock`**，它把 403 与其它错误分开：
  403 显示「无权访问」且**不给重试按钮**（重试一万次也还是 403），其余给重试。
  判据是 `ApiError.code === "access_denied"`，不是状态码。
- **`.then()` 必须配 `.catch()`**。`void p.then(...)` 与裸 await 是同一个坑
  （`ChatPage`、`AppShell` 的空间切换器都栽过）。
- 写操作后刷新用 hook 返回的 `reload`；就地改数据用 `setData`。

`useAsyncData` 刻意**不做缓存、去重、轮询**——只解决「失败时别停在骨架屏」这一件事。

### 测这条分支不能用 `page.route`

e2e 跑在 local 模式下，`LocalApiClient` 一个网络请求都不发，**`page.route` 永远不触发**
——用例会静静地跑在成功路径上然后在断言处失败，错误现场离真实原因很远。
失败要由 mock 自己给开关：`failReadsMock(page, "access_denied")`（见 `e2e/fixtures.ts`），
标记落在**独立**的 localStorage key 上，否则每个用例前的 `reset()` 会把它冲掉。
用例在 `e2e/13-load-errors.spec.ts`。

## 图标必须离线可用

图标走 `@iconify/react` 的 `<Icon icon="gravity-ui:xxx" />`，数据来自本地
`@iconify-json/gravity-ui`，在 `src/lib/icons.ts` 里 `addCollection` 注册，`main.tsx`
渲染前调用。

**关键前提：`@iconify/react` 只认显式注册进来的图标，没注册的一律去
`api.iconify.design` 取。** 连不上那个 CDN 时（内网、离线环境），「装了依赖但没注册」表现成
**整页图标一个不显示，且不报错、不占位**——icon-only 的按钮（对话页左栏那个「新建
对话」+）就此变成一块看不见的可点区域。只注册代码真正用到的 48 个（全集 799 个 435KB，
子集 24KB，省 94%）。

加图标就直接写 `<Icon icon="gravity-ui:foo" />`，然后 `npm run check-icons`
（已挂在 `npm run build` 前面）会告诉你补进 `icons.ts` 的 `USED`，或者名字拼错了。

### 三条只能靠「断网后图标还在不在」验证的坑

以下三种都**不报错**，`addCollection` 甚至照样返回 `true`：

1. **`icons` 这个导出是「整个图标集」不是「图标字典」。** 形状是
   `{prefix, icons, lastModified}`，单个图标在 `collection.icons[name]`，比直觉多一层。
2. **别把 `width` / `height` 透传进 `addCollection`。** gravity-ui 没设整集尺寸，读出来是
   `undefined`，而 `addCollection({..., width: undefined})` 会让 iconify **一个图标都不
   注册**却返回 `true`。实测带上这两个键 `listIcons()` 恒为 0，去掉就正常。
3. **图标名要真的存在。** `gravity-ui:robot` 上游根本没有（只有 `face-robot`），
   联网也画不出来。这条现在由 `check-icons` 拦住，并会提示形近的正确名字。

验证方式只有一个：把 `api.iconify.design` 掐掉再看页面。计数相等才算通过——
数 `<svg>` 有没有 `<path>` 子节点，不能只看元素在不在。

## 根路径 `/` 自动进上次访问的空间

`/` 不再无条件跳 `/workspaces`，而是先看本机记着的「上次在哪个空间」
（`lib/lastWorkspace.ts`，key `ama.lastWorkspace.v1`）。**`/workspaces` 本身不变**，
手输永远是列表页——两个地址是两个去处：`/` 是「带我回去干活」，`/workspaces`
是「让我挑一个」。把后者也接上自动跳转，用户就再也回不到列表页了
（侧栏切换器里那条「管理所有空间」正指向它，是从空间内回列表的唯一出口）。

**用 localStorage 而不是落库，这是判断的结果不是漏做。** 仓库里有两条相反的先例：
`prefs/storage.ts` 拿它当缓存，`app/OnboardingTour.tsx` **刻意不用**、状态落 PG。
后者之所以必须落库，是因为「同一个人不会被引导两次」是**正确性要求**；
而这条记忆丢了只是退回列表页让人自己选，是**可降级的偏好**。
落库还会把首屏押在一次网络往返上，而那恰恰是这个功能最不能付的代价。

四件事决定了它现在的形状，每一件都对应一种零报错的坏实现：

- **去处在渲染期同步算出来**（`router.tsx` 的 `RootRoute`）。做成「先渲染列表页、
  再在 effect 里跳走」两条断言照样绿，但用户会看见列表闪一下。判据不是「最后到了
  哪」，而是**列表页那个 heading 有没有进过 DOM**——用例 ② 用一个 500ms 观察窗盯着它。
- **有效性不在 `RootRoute` 里判，交给 `WorkspaceGate`。** 它本来就要按 key 取空间、
  本来就分得清「不存在」（null → missing）与「无权访问」（reject → forbidden）。
  在跳转前再 await 一次接口等于把首屏押在网络上，还多一份会漂移的判据。
- **⚠️ 同样是 403，两种来路要给相反的结果。** 自动跳来的静静回列表页（用户压根没
  要求进这个空间，把他没点过的空间的权限错误糊他脸上只会让人莫名其妙）；手输 /
  点收藏夹 / 别人分享的链接**仍然显示** `workspace-forbidden`——静默跳走会让他看到
  「点了没反应」，而「你不在这个空间」是唯一有用的那句话。区分靠 `location.state`
  而**不是 query 参数**：`?from=root` 会进地址栏、被收藏被分享，那条链接对别人是
  「自动跳转」语义，正好把这张表反过来。
- **打不开就把记忆忘掉**，否则用户每次进来都被送去同一个打不开的地方、再被弹回列表，
  **自己永远修不好**。⚠️ 只清 `missing` 与**确实 403** 的那半：一次 500 或断网也落到
  `forbidden`，照清的话服务端抖一下就抹掉用户的偏好，而他只会觉得「怎么不记得我了」。
  ⚠️ `forgetLastWorkspaceKey` 还要**比对 key 才清**——手输别人的空间撞上 403，
  不该把他真正在用的那条记忆一起抹掉。

写入放在 `WorkspaceGate` 里、依赖 `state.workspace.key` 这个**字符串**：这个组件是
`/w/:workspaceKey` 那一层的 element，**不随子路由重挂载**，所以天然是「进入/切换空间
时写一次」而不是每次路由变化都写。依赖整个 `state` 对象的话，`refresh()` 每次都会白写。

**多标签页最后写的赢，这是可接受的**；但刻意**不监听 `storage` 事件**——那会让 A
标签页切个空间把 B 也一起带走，而用户在 B 里可能正打着字。

读出来的值**必须过一遍 `validateWorkspaceKey`**：它会被拼进 URL，而 localStorage 是
用户改得动的，不校验就会拿一个坏值去访问 `/w/<非法串>`，报错不指向「本机存了个坏值」。

用例在 `e2e/35-last-workspace.spec.ts`，四条成对（无记录 / 有效 / 失效 / 手输
`/workspaces`），失效那条**拆成 null 与 403 两条**——它们走两条不同的代码路径，
合成一条会漏掉其中一半（实测变异验证时 403 那条红了、null 那条照样绿）。
⚠️ 造 403 只能用 `denyWorkspaceMock`，`page.route` 在 local 模式下拦不到任何东西。

## 未登录态：落地页 → 登录卡，两阶段

未登录时 `AuthGate` 渲染的是 `pages/LandingPage.tsx`（产品介绍 + 一个「登录 AMA」按钮），
**点了才**渲染 `pages/LoginPage.tsx` 那张扫码卡。三条：

- **阶段切换靠挂载，不要加 `enabled` 开关。** `LoginPage` 本来就是「挂载即发起设备码、
  卸载即取消轮询」的，所以推迟渲染＝推迟发起，点返回＝组件卸载＝现成的 cleanup 清掉
  定时器。从前一进来就发 SSO 请求，现在停在介绍页时一个请求都不发。
- **整页外壳归 `LandingPage`，`LoginPage` 只画一张卡。** 两处都写 `min-h-dvh` 会叠成两层壳。
- **e2e 里未登录态默认不可达**——mock 的 `getCurrentUser()` 恒定返回内置用户。要测就用
  `fixtures.ts` 的 `signOutMock()` / `completeLogin()`（标记落在**独立**的 localStorage key
  上，否则每个用例前的 `reset()` 会把它冲掉），见 `e2e/12-landing.spec.ts`。
  **`logout()` 刻意仍然不真的登出**：改它会让任何误触登出的用例之后全部跳登录页。

## 写不存在的 token，Tailwind 静默不产出

`text-on-accent` 与 `bg-danger-subtle` 曾在登录页与飞书接入页用了 4 处——**这两个类
HeroUI 和本仓库都没有定义**，而 Tailwind 对未知类**不报错、不产出任何 CSS**：
`tsc` 过、`build` 过、浅色下还正好看着是对的（accent 底 + 浏览器默认深色字），
**只有切到深色才暴露**——那时 accent 反转成近白，白底白字，按钮文字整个消失。

正确写法是 `text-accent-foreground` 与 `bg-danger/5`。判据不是「读着像不像」，而是
去产物里查一眼：

```bash
grep -c '\.text-accent-foreground{' web/dist/assets/*.css   # 0 就是这个类不存在
```

可用的 token 名以 `node_modules/@heroui/styles/dist/themes/shared/theme.css` 里那批
`--color-*` 为准（`accent-foreground` / `surface-secondary` / `muted` / `danger` 等），
**别按语义顺手造名字**。

## 移动端（iPhone H5 / 添加到主屏幕）

**地基在 `142d3585`**（⚠️ 那条 commit 的 message 说的是 trace 排序——并发会话把彼此的
message 抢串了，移动端的完整说明在 `8fbf4174` 的 body 里，而那条 commit 的内容
其实是 trace 的三个文件。**两条都别照 message 找，照文件找**）。

外壳的形状（`app/AppShell.tsx`）：

```
<div class="flex h-dvh overflow-hidden">     ← 定高，自己不滚
  <div class="hidden md:flex">侧栏</div>       ← 桌面常驻
  {mobileOpen && <抽屉 吃 safe-top/bottom>}    ← 窄屏，汉堡唤出
  <div class="flex flex-col flex-1">
    <header md:hidden 吃 safe-top>汉堡+空间名</header>   ← 固定顶栏，**在滚动容器外**
    <main class="flex-1 overflow-y-auto overscroll-contain">  ← 唯一滚动区
```

顶栏**必须留在 `<main>` 外面**。它原先长在里面，于是内容一滚就跟着滚走——窄屏等于
没有固定导航入口，而这件事不报错、只是「用起来不对」。

可用的 token（`index.css` 的 `@layer base`）：`--safe-top/bottom/left/right`
（安全区四边，`env(..., 0px)` 带 fallback）与 `--kb`（键盘高度，由
`lib/useVisualViewport.ts` 写，默认 0px）。

### 四条会静默坏掉的事

**`viewport-fit=cover` 是安全区的前提。** 不写它，`env(safe-area-inset-*)` **全部返回
0 且不报错**，于是灵动岛与 Home Indicator 的留白一行都不生效——照现象查会以为 CSS 写错。

**iOS Safari 不支持 `interactive-widget`**（MDN browser-compat-data PR #29064 标题即
「Safari iOS does not support interactive-widget viewport feature」；WebKit Bugzilla
259770 至今是 "Implement …"）。所以**键盘避让不能靠纯 CSS**，只能走 `visualViewport`。
`index.html` 里仍然写着那个属性——Android Chrome 认它，零成本渐进增强。
另外 iOS 上键盘弹起**不改变任何 vh 单位（含 `dvh`）**，别指望 `100dvh` 会缩。

**iOS 26 键盘收起后 `visualViewport.offsetTop` 不归零**（Apple Developer Forums
thread 800125 / 800154，WebKit Bugzilla 311821 / 317749）。算键盘高度时不夹
`Math.max(0, …)` 会得到负值，底部固定区被推出屏幕**且再也回不来**，零报错。

**横向溢出不冒泡到 `documentElement`，它停在最近的滚动容器上。** 所以断言「没有横向
溢出」只查 `documentElement.scrollWidth` 是假绿的：`<main>` 上有 `overflow-y-auto`，
按 CSS 规范一个轴非 visible 时另一个轴的 visible **计算成 auto**，溢出被 main 自己
接住了。变异验证（给 PageBody 加 `w-[500px]`）时那个版本照样全绿。要防「用户能左右
滑」就得查用户实际在滑的那个元素。

### 判「窄屏会不会溢出」只认实测，不认算术

2026-08-29 实测：**16 个页面全部零横向溢出**（overview / agents / chat / skills /
schedules / feishu / issues / runs / settings / usage / preferences / workspaces /
运行详情 / agent 详情，`doc` 与 `main` 都是 `402/402`，扫「`getBoundingClientRect().right
> 402.5`」零命中）。**所以现在不需要根级 `overflow-x: hidden` 兜底**——没有要治的病，
加上去只会把将来真的溢出藏掉（内容看不全，且 `scrollWidth > clientWidth` 这个唯一
判据被永久关掉）。

**读 vendor CSS 做算术会系统性高估近一倍。** 一次审计按
`.segment__item { padding-inline: 1rem; white-space: nowrap }` 估出 `ViewSwitch`
（5 项）≈400px、设置页 tab 条 ≈420px，报成头号溢出源。实测 `ViewSwitch` 整条
**236px、右边缘 386px**，5 项各 44/44/56/44/44px；设置页扫全页溢出容器**返回空数组**。
错因是**漏读 size 修饰类**（`ViewSwitch` 传了 `size="sm"`，`.segment__item--sm` 把
padding 压到 0.625rem、字号降到 `text-xs`，而压缩后的 CSS 是一整行，grep 命中基础类
就以为读完了），外加 flex 容器会压缩子项、中文按字符数估偏宽。

量宽度的探针顺手也该查**「该看见的东西是不是 0×0」**——那类缺口比溢出严重得多，
本轮就是这么发现对话页在窄屏无法新建会话的（见下）。

扫溢出容器时会遇到一个**假阳性**：`div` 报 `scrollWidth 85 > clientWidth 1`。那是
HeroUI `Select` 的隐藏原生 select（`1x1` 在 `@-1,-1`、`overflow:hidden`，给 a11y 与
表单提交用），不是 bug。判据是 `clientWidth < 50` 且位置在负坐标。

### ⚠️ 对话页在窄屏没有会话入口（未修完）

`/w/:key/chat` 的 `chat-new`（新建会话）与 `chat-list`（会话列表）在 402px 下**实测都是
0×0**，被 `ChatPage.tsx:297` 那个 `hidden w-64 ... lg:flex` 侧栏隐藏，而页面别处没有
替代入口。后果是**手机上无法新建对话、也看不到历史会话**。断点是 `lg:`(1024px)，
所以平板与小笔记本一起中招。

`AppShell` 是把桌面侧栏搬进抽屉（`:407-418`），ChatPage 是直接藏掉——需要同构处理。
修的时候 **testid 必须复用 `chat-new` / `chat-list`**（`e2e/09-chat` 有 2 条在找它们），
且注意复用侧栏 JSX 渲染两遍会让 testid count 变 2、撞 Playwright strict mode。

### e2e：iphone project

`playwright.config.ts` 有两个 project，靠 `testMatch` / `testIgnore` 分工：
`chrome` 跑 `e2e/` 下除 `mobile/` 的全部，`iphone` 只跑 `e2e/mobile/`。
移动端另写一组用例、**不改那 270+ 条桌面用例**——否则为了让它们在窄屏变绿，很容易
一路妥协到把移动端改回桌面布局。

⚠️ **`browserName: "chromium"` 与 `channel: "chrome"` 两个都要写。**
`devices["iPhone 16 Pro"]` 自带 `defaultBrowserType: "webkit"`，而本机只有 chromium。
只写 channel 会报 `browserType.launch: Unsupported webkit channel "chrome"`。

⚠️ **断言前先等外壳渲染**：`AuthGate` 挂载时 `app-scroll` 与顶栏都还不在 DOM 里，
而且那期间**整页是空的**——启动门（`AuthGate` / `WorkspaceGate`）的加载提示是
「延迟 300ms 才出现的无字 spinner」（`components/Page.tsx` 的 `StartupSpinner`），
快路径上一个可见元素都没有。实测有一条「无横向溢出」**在只有 spinner 的页面上通过了**
（一个只有 spinner 的页面当然不溢出），**空页面只会让这个陷阱更深**。
`e2e/mobile/23-mobile-shell.spec.ts` 的 `gotoShell()` 是这道门，照它写。

⚠️ **这个 project 测不出 iOS 的真实行为**：Chromium 里 `env(safe-area-inset-*)` 恒为
0、软键盘不弹起、`visualViewport` 行为不同、standalone 模式无法模拟。它守的是**布局
契约**（顶栏固定、无横向溢出、内容区独立滚动）。安全区、键盘、添加到主屏幕**必须真机验**。

## 事件视图与对话视图：同一份 RunEvent，两条**不共用**的管线

`?view=events` 与默认的对话视图读的是同一份 `RunEvent[]`，但**刻意不走同一条管线**：

| | 对话视图 | 事件视图 |
| --- | --- | --- |
| 数据层 | `lib/timeline.ts` 的 `buildTimeline()` | `lib/rawEvents.ts` 的 `buildRawRows()` |
| 工具调用 | `tool_use` + 结果**配对成一条** | **各占一行，不配对** |
| 条目数 | 比原始事件少约三分之一 | **等于**原始事件数 |
| 详情 | 行内展开 | 侧栏（窄屏抽屉） |

分开的理由：事件视图的语义是「忠实展示方舟返回的原始 event」（DESIGN.md 3.6.3），
要能与库里 `run_events` 逐条对上；而配对是**对话视图**的正确形态。让事件视图走加工管线
是概念错位，而给 `buildTimeline` 加「不合并模式」会让那 966 行再多一条贯穿全程的分支。

**由此有两件事在事件视图里不存在，别去补：**

- **工具调用的派生耗时**。它是「回执时刻 − 发起时刻」，是配对的产物。单条事件上
  硬要显示耗时只能编。唯一算得出的是 `span.model_request_end`——它带方舟给的显式
  `model_request_start_id`，是唯一有关联字段的一对。
- **「已批准 / 待你确认 / 执行中」**。都要把多条事件合起来判，归对话视图。
  事件视图给原始值（`evaluated_permission: ask`）与独立的 `user.tool_confirmation` 行。

`TimelineToolbar` 两边共用，但**计数必须各按自己的口径给**（`ToolbarCounts`）：
用 `computeStats()` 那份（按配对后的条目数）会出现「筛选器说 tool 有 8 条、点开显示
15 条」，不报错，只让人怀疑筛选坏了。

### 筛选区收进 Popover：三条只有跑起来才知道的事

筛选轴原先**平铺六行**（搜索 / 快速筛选 / 事件域 / 语义类别 / 类型折叠 / 派生条件），
在事件列表**上面**堆了大半屏。收进一个 Popover 之后是一行（实测 32px）。

**收的是控件，不是状态。** 已选条件以 chip 留在行上，每个能就地摘掉——
「当前筛掉了什么」若也藏起来，用户会看着一个不完整的列表却找不到原因。
触发器上带徽标给出生效条件数（**关键词不计入**：它自己有输入框摆在外面）。

⚠️ **徽标要按「偏离默认的档数」算，不是按选中数。** 默认是「8 类里显示 7 类
（trace 默认隐藏）」+「4 域全开」，按选中数算的话默认态就显示 11。

三个坑：

1. **用 Popover 而不是 Disclosure。** Disclosure 展开会把下面的内容整体推下去，
   于是「调筛选」这个动作让正在读的列表跳位；Popover 是浮层，列表不动。
2. **弹层的 dismiss 层会拦掉外面的一切点击。** 点完弹层里的控件想再点列表行、
   `filter-reset` 或搜索框，**必须先关弹层**。不关的报错是
   `<body>…</body> intercepts pointer events` + 60 秒超时，完全不指向「弹层还开着」。
   e2e 里配了一对 `openFilters()` / `closeFilters()`（`e2e/fixtures.ts`）。
3. **`openFilters()` 必须幂等。** 弹层开着时它自己盖住了触发器，再点一次会被
   「弹层里某个碰巧压在触发器上的 ToggleButton」拦住——报错指向那个按钮的文本
   （如 `session.thread_created`），一样不指向真实原因。所以它先判 `isVisible()`。

顺带一条：一行里混用 HeroUI 的 `SearchField` 与 `size="sm"` 的按钮会差一档高度
（前者 md 是 40px），所以搜索框用了原生 `input` + field token 手搓成 32px。

### 删掉了那个「排序」下拉——它从来没生效过

`applyRawFilter` 从来没读 `filter.sort`，切到「按耗时降序」不改变任何顺序
（调用树的耗时排序是 `TracePanel` 自己的 `useState`，与它无关）。
而在「一条 event 一行」的语义下**单条事件本来就算不出耗时**，所以它不是
「还没接上」，是**不该存在**。连同 `?sort=`、`TimelineSort`、`TIMELINE_SORTS`
一起摘掉。**一个切了没反应的下拉比没有下拉更糟**——它让人以为自己排错了。

同一次清掉的死代码：`applyFilter()` 与 `buildHaystack()`（吃 `TimelineItem` 的那两个）
——事件视图是它们唯一的调用方，改版后没人再用。

### `ModelUsage` 的四个计数字段都是可选的

**方舟会省略取值为 0 的字段。** 线上一条 24 次模型请求的运行里，1 条 `model_usage`
只给了三个字段、没有 `cache_creation_input_tokens`（那次确实没写缓存）。

按必填声明的代价是**汇总直接变 NaN**：`usage.x += e.model_usage.x` 里 `undefined`
一进来，累加值就永久是 NaN，页面上显示成「写入 **NaNM**」——一条缺字段的事件
毁掉整个运行的用量摘要，且不报任何错。所以类型上四个字段都是 `?`，累加处一律 `?? 0`。

### `processed_at` 是唯一的时间来源，它有四个坑

实测本地 PG 2439 条真实事件：

1. **`doc` 列里那份在真方舟数据上全是 null**，只有 `raw` 列有值（种子数据才两边都有）。
2. **小数位宽不固定**：9 位 2360 条、4 位 76 条、6 位 2 条。不能按定长切串。
3. **两种时区形态并存**：`+08:00` 2362 条、字面 `Z` 76 条。所以**字符串比较会给出错误
   顺序**——必须先解析成绝对时刻再比。
4. **同毫秒碰撞是常态**：单个 run 662 条事件只有 400 个不同毫秒，而 JS 的 `Date`
   只到毫秒，纳秒**直接丢**。

所以排序是 **(毫秒时刻, sequence)** 的字典序，`processed_at` 为 null 的沉底
（未知 ≠ 0，当 0 会让它冒到最前面看着像最先发生）。

**⚠️ `sequence` 不等于真实时间顺序。** 这条是端到端跑真方舟才发现的：一条 218 事件的
真实运行里，`sequence: 213` 的 `processed_at` 是 `.429765508`，比 `sequence: 2` 的
`.429772939` **更早**。只按 sequence 排会让 **211/218 行落在错位置**。
按时刻排不是洁癖，是必需。

### 同毫秒的 sequence 兜底在 UI 上**不可观测**

那句 `return a.sequence - b.sequence` 防的是「入参不按 sequence 有序」，但
后端 `order by sequence`（`store_pg.go`）与 mock 的 `sort` 都保证入参已有序，
而 `Array.sort` 是稳定的——于是把它改成 `return 0`，**e2e 照样全绿**（变异验证实测）。

留着它代价为零（`TimelineItem` 那边曾因 `undefined - undefined = NaN` 真的乱过序），
但**别写一条断言声称自己测到了它**：那是恒真的假绿。相关的种子形状（`shuffledSameMillis()`
把三条同毫秒事件在数组里乱序摆放）也在注释里写明了这一点。

## 飞书接入

已改成设备码流程：`/w/:key/feishu/connect` 三步走，**删掉了手填五个凭证字段的表单**。

轮询逻辑收敛在 `src/lib/useRegistrationPoll.ts`（另一处是登录页的设备码轮询，递归
`setTimeout` 而非 `setInterval`，卸载即取消）。就绪度诊断在
`src/components/FeishuReadiness.tsx`。

## 人的头像：统一走 `UserAvatar`，别再各写一遍兜底

「显示这条东西是谁建的」现在有一个通用组件：`src/components/UserAvatar.tsx`。
小圆头像 + 悬停出名字与邮箱，调用方**只需要给邮箱**：

```tsx
<UserAvatar email={a.createdBy} profile={profiles[(a.createdBy ?? "").toLowerCase()]}
            label={t("agent.createdBy")} />
```

### 四种状态，其中一种是这次新增的

前三种是既有纪律（`<Avatar.Image src>` + `<Avatar.Fallback>` 首字母，
Radix 只在图片 loaded 之后才渲染 `<img>`，所以「没有这个字段」「还在加载」
「URL 挂了」自动统一落到首字母）——**绝不要写 `avatarUrl && ...` 的条件渲染**，
那反而会在加载失败时留一个空圆。

⭐ 第四种是 `UserAvatar` 存在的理由：**根本不知道是谁**。

**存量 agent 永远补不上创建者**——`createdBy` 是后加的字段，在那之前建的 agent
当时没记，jsonb doc 里没有、方舟侧也没有，无从考据。所以「键不存在」是一种
**会长期存在的合法状态**，不是数据坏了。界面上渲染成**问号头像**、
tooltip 明说「未记录创建者」：

- 不能显示空白——一列头像里缺一个，看着像渲染坏了；
- 不能猜一个人填上去（比如空间 owner）——猜错的归属会被当成事实读，且不报错。

### 档案要批量查，别做成 N+1

`useUserProfiles(emails)`（`src/lib/useUserProfiles.ts`）把整页的邮箱去重后
**一次查完**，返回一张查找表。查不到的邮箱**不在返回值里**，那是常态
（那个人没登录过），直接把 `undefined` 交给 `UserAvatar` 兜底即可。

⚠️ **展示名与头像不存进实体**。`agents` 是 `doc jsonb` 全量落库，把 name/avatar
挂到 `Agent` 上会让它们跟着写进库，于是「建 agent 那一刻的名字」被永久冻结，
那个人改名换头像之后列表上还是旧的。`WorkspaceMember` 自带那两个字段是被上游
通讯录逼的（没有按邮箱批量查的接口），**users 表没有那个限制，别照抄**。

### ⚠️ 悬停要用真实鼠标轨迹，`locator.hover()` 可能不触发

React Aria 的 `useHover` 会忽略「瞬移」型的合成事件。手动验证 tooltip 时要
`mouse.move` 先落在旁边、再带 `steps` 移过去，否则 tooltip 根本不出来——
**那不是组件坏了，是测法错**。

## 约定

- `@` 别名指向 `web/src`。
- TypeScript 严格模式，且开了 `noUnusedLocals` / `noUnusedParameters`——
  **未使用的变量会直接编译报错**。
- 数据模型的命名：镜像方舟的用 `snake_case`，AMA 自有的用 `camelCase`。

## 跑起来

**日常用仓库根的 `./dev.sh`**：它构建前端、用 `go:embed` 嵌进后端、起单进程监听
`0.0.0.0:5273`（重复执行即重启）。前后端同源，不用管代理目标、也没有跨域。
代价是**没有 HMR，改完前端要重跑 `./dev.sh`**（约 9 秒；只改 Go 用 `--skip-web`，约 1 秒）。

⚠️ **`./dev.sh` 起新进程前会按端口清理旧进程**：在另一个 worktree 里不带 `PORT=` 跑，
会把占着 5273 的那个服务 kill 掉（且零报错）。多个 checkout 并行时一律
`PORT=<别的端口> ./dev.sh`，`--stop` 也要带同一个 `PORT=`。

`web/scripts/dev.sh` 那条 vite 路子仍然保留，改前端时想要 HMR 就用它 ——
但它只起前端，`/api` 靠 vite 代理转给另一个终端里的 `cmd/amad`，
端口不对时用 `AMA_PROXY_TARGET` 覆盖。

```bash
npm install
npm run build                # check-icons && tsc -b && vite build
npm run typecheck
npm run check-icons          # 图标对账：用到的都存在且已注册（离线可用）
npm run e2e                  # Playwright，116 个用例，用系统 Chrome，自己起 5274 上的 server
./scripts/dev.sh             # 只起 vite（有 HMR）——别直接 npx vite，见 environment.md
```

`./scripts/dev.sh` 的端口可以用 `PORT` 覆盖（`PORT=5280 ./scripts/dev.sh`）。

`npx vite` 直接跑会 EMFILE 挂掉，原因和解法见 [environment.md](environment.md)。
