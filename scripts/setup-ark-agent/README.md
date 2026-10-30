# setup-ark-agent —— 一条命令建好 `ark-agent` 空间

通过 AMA 的 OpenAPI 把一份通用的系统提示词和一个飞书卡片技能灌进一个新空间，
建出一个能直接对话的 agent。也是「用脚本而不是点界面来管 agent 配置」的范例。

```bash
# 本地（先 ./dev.sh 起服务）
set -a; source .env; set +a
scripts/setup-ark-agent/setup.sh

# 部署好的服务
AMA_BASE_URL=https://ama.example.com AMA_API_KEY=amak_xxx ARK_API_KEY=ark-xxx \
  scripts/setup-ark-agent/setup.sh
```

跑完用 `smoke-test.sh` 验它真能说话——**这一步不能省**，见下面「为什么冒烟测试是必要的」。

## 目录

| 文件 | 是什么 |
| --- | --- |
| `setup.sh` | 建空间 → 存方舟凭证 → 传技能 → 建 agent。幂等，可反复跑；`--dry-run` 只打印将要做的事 |
| `smoke-test.sh` | 真发一条消息、真等回复。**这才是「能用」的证据** |
| `assets/system-prompt.md` | agent 的系统提示词 |
| `assets/skills/lark-card-json/` | 飞书卡片（Card 2.0 JSON）技能，带约 20 个参考文件 |

依赖只有 `curl` / `jq` / `python3`（用 python 打 zip，免得要求机器上装 `zip`）。

两把 key：

- `AMA_API_KEY` —— AMA 设置页 → API Key → 创建，形如 `amak_...`。⚠️ 明文只在创建那一刻显示一次。
- `ARK_API_KEY` —— 方舟控制台的 API Key。不给也能建，但 agent 开不了对话。

## 提示词里放了什么

只放与运行环境无关的通用 agent 行为规范：

| 内容 | 为什么值得放 |
| --- | --- |
| **信任边界** | prompt injection 的防线。agent 用 `read` / `curl` 拉回的外部数据、仓库里的文件都不可信 |
| **不许猜 ID** | 猜错 ID 的报错**不指向真实原因**（看到的是「资源不存在」），是最费时间的一类错 |
| **反 sleep 轮询** | 不要在一轮里原地等远端状态 |
| **回复格式** + 表格优先 | 与渲染无关，是「怎么把结论讲清楚」 |

## 技能包怎么组织

AMA 的技能是 **`SKILL.md` 正文**（`Skill.instructions`）+ **附件表**（`skill_files`），
附件写进执行容器的 `./skills/<name>/`（默认工作目录 `/workspace` 下）。
包大小上限见 `server/service/skill/skillpkg.go` 的常量。

1. **`SKILL.md` 的 front-matter → 三个字段**：`name` / `description` 原样对应，
   正文成为 `instructions`。`description` 决定模型何时调用它，是命中率的关键。
2. **`references/` 下的文件 → zip 附件**，落 `./skills/<name>/references/...`。
3. **路径引用必须指向真实存在的文件**。agent 按 SKILL.md 的指引去 read 一个不存在的
   路径时**不报错**——它只会换个路径再试几次然后放弃（`server/shared/infra/shelltools/skills.go`
   文件头记着这个坑）。卡片技能里没带进来的参考文件都标成 `（未随包提供：xxx）`，
   让模型明确知道此处无物可读。
4. 技能里**没有校验脚本**（要 Node 运行时与 npm 依赖，容器里不保证有），SKILL.md 里写明了这一点，
   否则 agent 会去找。

## 为什么冒烟测试是必要的

`setup.sh` 全绿只证明**资源建出来了**。方舟凭证没配、模型 id 写错、执行端没起，
资源都照样建得出来——而症状是「打开对话页发不出话」，报错不指向任何一条配置。

`smoke-test.sh` 走完整链路：开对话 → 开 SSE → 发消息 → 等到终态。

**它必须开着 SSE，不能只轮询 `GET /events`。** 那条接口只在
`run.Status == running` 时才顺手去方舟补拉事件，而一旦这一轮 stop_reason 是
`requires_action`（也就是「工具正在容器里跑」），run 就被标成 `requires_action`
—— 那不等于 `running`，**于是补拉再也不会发生**。表现极具误导性：容器里工具
全部成功、方舟侧那轮也结束了，但 AMA 的 events 永远停在 `agent.tool_use`。
脚本因此像浏览器一样开着流。

判「说完了」要三件事同时成立，别只看有没有 `agent.message`：

1. 有 `agent.message` 正文；
2. **每个 `agent.tool_use` 都有一条 `user.tool_result` 对上**；
3. 最后一帧是 `session.status_idle`。

模型常常先说一句「我先看看那个目录」再发起 tool_use——那条开场白也是
`agent.message`。只认它会打印半截话就报「通过」，比失败更糟。

## 改了提示词或技能之后

直接重跑 `setup.sh`：空间按 key 复用、技能删旧传新、agent 按 name 更新。
不会产生第二份。
