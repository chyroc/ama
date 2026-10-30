# lark —— 飞书开放平台接入层

只覆盖「自助注册一个飞书应用」这条链路：**设备码流程**（OAuth 2.0 device flow）加
注册完之后的**应用巡检**。

业务编排（会话、鉴权、幂等、就绪度）在 [`server/feishu`](../../feishu/)，本包只做协议翻译。

## 用法

```go
c, err := lark.New(lark.Config{})   // 零配置即国内域
if err != nil {
    return err
}

// 1. 发起：不需要任何凭证
ch, err := c.Begin(ctx, lark.BeginSpec{AppName: "AMA 助手"})

// 2. 把 ch.VerificationURL 交给用户去点，然后按 ch.Interval 轮询
ticket, err := c.Poll(ctx, ch.DeviceCode)
if ticket.Status == lark.PollOK && ticket.AppID != "" {
    // 拿到 ticket.AppID / ticket.AppSecret
}

// 3. 巡检：装好之后还差什么
token, err := c.TenantAccessToken(ctx, ticket.AppID, ticket.AppSecret)
cfg, err := c.InspectApp(ctx, token)
```

可运行的完整示例见 [`example/main.go`](example/main.go)，它会真的注册一个应用。

## 设计取舍

**手写 HTTP，不封 SDK。** 这条链路用的接口官方 Go SDK 一个都没有。

**返回值不做二次 DTO 映射。** `Challenge` / `Ticket` / `AppConfiguration` 就是本包的
对外形状，字段名贴着协议但去掉了协议的历史包袱（比如 `expires_in` / `expire_in` 两种
写法在这里统一成一个 `ExpiresIn`）。

**协议状态不是错误。** `Poll` 返回 error 只代表「这次请求没打通」；「还在等授权」
「已过期」「被拒绝」都是正常返回值里的 `PollStatus`。把它们混成 error 会让调用方
分不清该重试还是该放弃。

**`Config.AccountsURL` / `OpenURL` 是测试注入口。** 它们存在的主要理由是让单测能用
`httptest` 起假飞书，从而全部离线。

## 已知边界

- **不能创建/修改应用配置**：本包只能读（巡检）和申请审批。改事件订阅、改权限声明
  都得用户自己去开放平台操作——所以 `server/feishu` 的 readiness 才要把「缺什么」
  列清楚。
- **巡检要 `application:application:self_manage` 权限**。没有它只能读到权限列表，
  读不到版本和回调配置，此时 `AppConfiguration.Inspectable` 为 false，**其余布尔字段
  不可信**，不要拿它们去判「用户没配置」。
- **不缓存 tenant token**。巡检是低频操作，且 `server/feishu` 已按应用缓存了整份
  巡检结果，这里再加一层只会让失效逻辑更难推理。

## 踩过的坑

**注册接口的协议状态带 HTTP 400。** 这是最容易写错的一处：`action=poll` 在「还在等
用户授权」时返回 **HTTP 400** + `{"error":"authorization_pending","code":20094}`，
设备码非法同样是 400。按状态码判成败会把「还在等」当成请求失败，整条轮询链路直接断掉。
所以 `postForm` 先解析响应体、只在响应体压根不是 JSON 时才看状态码。

这个坑**离线单测抓不到**（假服务器默认回 200），是真连了一次飞书才暴露的。现在
`TestPollTreatsHTTP400AsProtocolStatus` 用真实状态码把它锁住了。

**`invalid_grant`（code 20079）必须单独成一档。** 设备码被伪造或损坏时返回它。
不单独映射这个码的话，会掉进「未知错误」分支去尝试换账号域重试——而域名根本不是
问题所在，白跑一轮之后仍然失败，还把真实原因盖成了「协议错误」。

**`expires_in` 与 `expire_in` 两种写法都要收。** 实测返回前者，协议文档写后者。
只认一个的话会把有效期读成 0，会话一创建就被判过期。

**`addons` 的编码是 JSON → gzip → base64url，且 Go 的编码结果是确定的。**
Go 的 `gzip.Writer` 头部 mtime 写零，所以同样输入逐字节一致，单测可以直接断言最终
字符串。（Python 的 gzip 会嵌当前时间戳，拿它生成期望值会得到一个每次都变的串。）

**国内 / 国际是两套独立租户体系，用户往往说不清自己在哪边。** 猜错域的表现不是报错，
而是**一直 poll 到 OK 但永远拿不到凭证**。`Ticket.TenantBrand` 会暴露真实归属，
纠正逻辑在 `server/feishu`。

## 取消息内容：两个「HTTP 200 但拿到的是假内容」的坑

`fetch.go` / `expand.go` 负责把卡片与合并转发展开成 agent 读得懂的文本。
下面两条都是**实测**出来的（真飞书 + 本地 `run_events` 里的真实事件），文档没提，
而且**两种情况都是 `HTTP 200` + `code 0`，不会有任何报错告诉你拿错了东西**。

**一、合并转发的 `content` 是个固定哨兵串，连 JSON 都不是。**

```
"content": "Merged and Forwarded Message"
```

被转发的那几十条消息一个字都不在里面。不特判的话 `decodeContent` 会走
「解不出对象说明它本来就是纯文本」那一支，把这句英文原样当成用户说的话交给 agent
——零报错，agent 拿着一句废话回答。子消息要靠

```
GET /im/v1/messages/{id}
```

取回，它们**就在同一次响应的 `data.items` 里**，是「1 条父 + N 条子」的**扁平数组**
（不是嵌套），靠 `upper_message_id` 认父子。**不需要第二次调用**，也**没有**
`container_id_type=merge_forward` 这种东西——那个参数只属于列消息接口，且只认 `chat`。

**二、卡片的 `content` 默认是「降级渲染态」，不是发出去的那张卡。**

实测一条卡片消息推过来长这样：

```json
{"title":null,"elements":[[{"tag":"img","image_key":"img_v3_…"},
 {"tag":"text","text":"请升级至最新版本客户端，以查看内容"}]]}
```

真正的正文一个字都不在里面。**唯一的开关是 `card_msg_content_type=user_card_content`**：

```
GET /im/v1/messages/{id}?card_msg_content_type=user_card_content
```

加上它，同一条消息回的是
`{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"收到，我在的 👋…"}]}}`。
**漏了这个参数不会报错，只会静默拿到「请升级客户端」那段占位文字**——那比
`[卡片]` 更糟，因为它看起来像真内容。`fetch_test.go` 有一条专门钉住这个 query 真的
进了 URL。

**卡片有三种形状要认**（判据是元素长什么样，不是 `schema` 字段——降级态压根没有）：

| 形状 | 正文在哪 |
| --- | --- |
| 2.0 | `body.elements[].content`（`tag: markdown`） |
| 1.0 | `elements[].text.content`（`tag: div`） |
| 降级态 | `elements[][]` 行×段，段里是 `text`（与 `post` 同构） |

**展开有长度上限**（`maxForwardChildren` / `maxChildTextLen` / `maxExpandedTotalLen`），
截断时会在文本里明说。理由是那些内容**会永久落进 RunEvent**，而实测单条子消息正文
就能有 3KB+（大半是一个 base64 的日志检索链接）。**展开不出来时返回空串**，
调用方据此保留原来的占位符——绝不能把 `[卡片]` 变成空字符串，那样 agent 连
「这里有个东西」都不知道。

**应用身份拿不到发消息的人的姓名。** `contact/v3/users` 与 `users/batch` 都只回
`open_id` / `union_id`，消息里的 `sender` 块也没有 `name`。所以 `FetchedMessage`
刻意不留姓名字段——留一个永远为空的字段会让调用方以为「有时候能拿到」。
群名倒是拿得到（`GetChat`）。

## 取历史：`ListMessages`

```
GET /im/v1/messages?container_id_type=chat|thread&container_id=…&sort_type=…
```

`container_id_type` **只有 `chat` 与 `thread` 两个值**（`chat` 同时覆盖群与单聊，
实测单聊传 `chat` + `oc_…` 能取到）。三种范围由调用方按会话形态选，
选错**不报错**，只是 agent 读到一堆无关消息或漏掉真正的上文：

| 形态 | 容器 | 排序 |
| --- | --- | --- |
| 话题群 / 群内话题 | `thread` + `thread_id` | 正序，取全部 |
| 普通群 | `chat` + `chat_id` | 倒序，取最近 N |
| 单聊 | `chat` + `chat_id` | 倒序，取最近 N |

⚠️ **话题接口返回的第一条就是开题的根消息**（实测 `root_id` 为 null）。
所以**不需要**再从群主流里过滤根消息——那样做会白拉一次群主流，还把别的话题的
噪音带进来。

⚠️ **单页拿不全，`ListMessages` 会翻页但有硬上限。** 实测 `page_size=2` 时响应里
`has_more=true` 且带回 `page_token`。只取单页却对外号称「取全部」，会在话题超过一页时
**静默少给上文**，而范围说明还写着「全部」——等于骗 agent 说它读完了。
（这是 code review 抓到的真问题。）现在循环取到 `has_more=false`，
上限由 `maxPages` 控制：话题 5 页、「最近 N 条」1 页。
⚠️ **后续页失败时保留已拿到的页**——少一点上文远好过完全没有上文。
⚠️ **停止判据同时看 `has_more` 与 `page_token`**，只看 token 的话某些实现在最后一页
仍回一个 token，循环停不下来。

⚠️ **取历史同样要带 `card_msg_content_type`**：历史里混着卡片很常见（机器人发的
告警就是卡片），不带的话每张卡片都会变成「请升级至最新版本客户端」。

⚠️ **读群历史除基础读权限外还要 `im:message.group_msg`，缺了回 230027。**
那个权限按应用授权，每个用户注册的应用要各自申请——所以**这个接口失败是常态而非
异常**，调用方必须降级（`feishu/larkcontext.go` 里那条只打一行 Warn 就走）。

⚠️ **群事件（`msg_type: system`）要整条跳过。** 改群名、拉人进群、建话题群都是这个
类型，它们**不是任何人说的话**。不跳的话会渲染成 `用户：[system]`——**两处都错**：
content 里根本没有 `text` 字段（只有 `{template, from_user, to_chatters}`），
所以只能得到占位符；而它的 `sender` 块是**四个空串**，会被默认成「用户」。
于是模型看到「有个用户说了句 [system]」，纯噪音，还可能让它以为自己漏解了什么去追问。
判据用 `msg_type` 而不是「正文像不像占位符」——后者是字符串匹配，用户自己打一句
`[system]` 就会被误删。

⚠️ **`sender_type` 为空时不能默认成「用户」**，那是把「不知道谁说的」断言成
「一个真人说的」，而模型看不出这是猜的。现在标「某人」。

`ExpandHistory` 负责渲染：一律输出**时间正序**（倒序取回来的会翻回来），
每行标出「用户：」/「机器人：」——**只能标到这个粒度**（拿不到真名，见上一节），
但这正是 agent 需要的：它得知道哪些话是自己以前说的。

## 测试

```bash
go test -race ./shared/infra/lark/
```

全部离线，不连飞书、不需要凭证。假飞书用 `httptest` 起，靠 `Config.AccountsURL` /
`Config.OpenURL` 注入。真实连通性验证靠 `example/main.go`——需要人去授权页点同意，
不适合进 CI。

⚠️ **`expand_test.go` 里那几个 content 字符串是从真飞书原样抄回来的**，不是照文档
编的。这件事有必要说明：这两种消息的形状与直觉差得远（见上一节），
按文档猜出来的 fixture 会让实现「测试全绿、线上解不出」。
