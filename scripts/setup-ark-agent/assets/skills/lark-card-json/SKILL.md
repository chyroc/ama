---
name: lark-card-json
description: 生成飞书/Lark 互动卡片的 Card 2.0 JSON。用户要求生成或设计飞书卡片、输出互动卡片 JSON、做报表/仪表盘卡片、通知卡片、审批卡片、告警卡片、带按钮的操作卡片时使用。只产出 JSON 对象本身，不负责发送。
---

## 这个技能产出什么

**一份合法的 Card 2.0 JSON 对象**，仅此而已。不要输出 webhook 外层包装
（`{"msg_type":"interactive","card":...}`），也不要试图自己发送——投递由调用方负责。

## 附件在哪（先读这条，否则会读不到文件）

本技能的参考文件在工作目录下（默认工作目录是 `/workspace`）：

```
./skills/lark-card-json/
├── references/content.md            内容与文案规范（写什么、口径、来源引用）
└── references/schema/               字段定义，按需读
    ├── components-index.md          ← 组件路由总表，不确定用哪个组件时先读它
    ├── config-card-root.md          卡片根对象（schema / config / header / body）
    ├── config-behaviors.md          按钮行为（callback / open_url / 二次确认）
    ├── config-shared-primitives.md  跨组件共享的文本/图标/URL 原语
    ├── layout-nesting.md            谁能嵌进谁
    ├── layout-containers.md         容器总览
    ├── component-*.md               单个组件的字段（title / table / chart / button …）
    ├── container-*.md               分栏、折叠面板
    └── primitive-color.md, primitive-icon.md
```

**read 时用这个路径**，比如
`read ./skills/lark-card-json/references/schema/components-index.md`
（默认工作目录下它等价于绝对路径
`/workspace/skills/lark-card-json/references/schema/components-index.md`，两种写法都行）。

⚠️ **也没有校验脚本。** 卡片 JSON 的合法性靠你自己按参考文件保证；
想过一遍语法就用 `bash` 跑 `python3 -m json.tool`。

**按需读，不要一次全读。** 正常流程是：先看 `components-index.md` 定位组件 →
只读命中的那一两个 `component-*.md`。全读会挤掉干活的上下文。

标着 `（未随包提供：xxx）` 的引用表示**那份文件没随包带进来**——不要去猜它的路径
反复 read，直接按已有信息做，或者告诉用户这个组件的字段细节手上没有。

## 最小可用骨架

```json
{
  "schema": "2.0",
  "config": { "compact_width": false },
  "header": {
    "title": { "tag": "plain_text", "content": "标题" },
    "template": "blue"
  },
  "body": {
    "elements": [
      { "tag": "markdown", "content": "正文，支持 **加粗**、列表、链接。" }
    ]
  }
}
```

## 做之前先想清楚三件事

1. **这张卡片要回答用户什么问题**——先定信息层级，再选组件，不要反过来。
2. **需不需要交互**：纯展示就别加 `behaviors`；要按钮就去读 `config-behaviors.md`。
3. **数据是真的还是示例**——这条最容易出错，见下面。

## 内容规范（硬约束）

完整规则在 `references/content.md`，动手前读一遍。最要紧的几条：

- **禁止编造指标、数据、统计值、链接、ID、负责人、日期。** 用户没给且不可验证的，
  用占位符：`TODO_METRIC` / `TODO_LINK` / `TODO_OWNER` / `TODO_ID`。
- 用了示例或模拟数据，**第一屏必须标注「示例数据」**——不要把它写成真实线上状态。
- 资讯、摘要、推荐类卡片**必须给来源链接**；没有可验证来源就不写，标「来源待补充」。
- 按钮和链接文案用具体动词、最多 3 个词，脱离上下文也能懂。
  禁用 `OK` / `Submit` / `Click here` / `Continue` 这类无信息文案。
- 趋势、对比、占比、排行**用 `chart` 或 `table` 表达**，
  禁止用 Markdown 或 ASCII 字符伪造图表。

## 交付前自检

- [ ] JSON 能被解析（`bash` 里 `python3 -m json.tool` 过一遍最快）
- [ ] `schema` 是 `"2.0"`，`body.elements` 非空
- [ ] 每个组件的 `tag` 在参考文件里确认过，不是凭印象写的
- [ ] 所有编造风险点都用了 `TODO_*` 占位符，没有假装真实的数字
- [ ] 示例数据已标注
- [ ] 按钮文案是具体动词
