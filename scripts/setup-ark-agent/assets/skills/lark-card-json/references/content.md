# 卡片内容规范 (Content)

本文件是 lark-card-json 技能的**内容层**，与 `references/schema/`（Schema 层）平级，统一约束「写什么文字、用什么口径、信息是否真实、来源怎么给」。

职责边界：

- 本文件只管**内容与文案**，不管 schema 字段（见 `references/schema/`），也不管视觉风格。
- `SKILL.md` 不再重复这里的内容规则，需要时按路径引用本文件。

## 信息真实性

- 禁止编造指标、数据、统计值。用户未提供且不可验证的数字，不写成真实值。
- 缺失指标使用占位：`TODO_METRIC`、`—` 或 `metric to confirm`；不要用看似真实的假数填充。
- 示例 / 模拟数据：只要数据不是用户提供或不可验证，**第一屏必须**标注「示例数据」或「模拟数据」；禁止把示例 / 模拟数据写成真实线上状态。
- 未验证信息统一使用 `TODO_*` 占位符：链接 `TODO_LINK`、负责人 `TODO_OWNER`、图片 `TODO_IMG_KEY`、ID（open_id / chat_id / 单号等）`TODO_ID`、口径 `TODO_METRIC_SCOPE`。
- 禁止编造真实来源链接、URL、`open_id` / `chat_id`、审批单号、负责人、真实日期或外部系统标识。

## 来源与引用

- 资讯、咨询、新闻、行业动态、每日摘要、推荐阅读类卡片**必须**给出来源链接；没有可验证来源时不编造，标注「来源待补充」或只输出可验证内容。
- 来源链接二选一排版，除非用户明确要求同时展示：
    - **正文内嵌**：在对应资讯标题或句尾直接放来源链接，如 `**Google I/O 明天开场**（[Google Developers](https://example.com)）`。
    - **末尾来源模块**：最后增加「来源」模块，用数字序号列表纵向排布，如 `1. [Google Developers：Get ready for Google I/O 2026](https://example.com)`；正文中可用 `[1]`、`[2]` 对应来源序号。
- 末尾来源模块不要把多个链接用 `｜`、逗号或空格挤在同一行；每条来源单独一行，标题可读、链接可点击。
- 资讯正文优先写「发生了什么 + 为什么重要 + 建议关注什么」，避免只堆标题或来源列表。

## 文案 (Copy)

- 按钮、链接文案使用具体动词，最多 3 个词，脱离上下文也能理解；避免 `OK`、`Submit`、`Click here`、`Continue` 这类无信息文案。
- 链接文字必须能独立理解，不依赖上下文猜测目标。
- 优先具体、声明式表达，避免营销空话。禁用文案：`Click here`、`Oops!`、`Something went wrong`、`Unleash`、`Supercharge`、`Transform`、`Elevate`、`Empower`、`Next-generation`、`Seamless integration`。
- 列表（bullet）只用于真实步骤、待办、来源、日志，或无法扫描成 surface 的密集事实；表达型内容优先视觉化区块（具体视觉手法按卡片主题自定）。

## 数据可视化诚实性

- 趋势、对比、占比、排行等用户明确要求的图表，优先用 `chart` 或 `table` 表达；**禁止**用 Markdown / ASCII 字符伪造图表替代。

## 内容门禁 (preflight)

输出前自检（schema 合法性按 `references/schema/` 的字段定义自检，本节只管内容）：

- `no_invented_metrics`（error）：未提供的数字必须标注为样例数据或 `TODO_METRIC`。
- `sources_for_news`（error）：新闻和推荐类卡片必须包含可验证来源，或显式来源占位。
- `sample_data_labeled`（error）：示例 / 模拟数据已在第一屏标注。
- `no_ascii_chart`（error）：用户要求的图表未用 Markdown / ASCII 伪造。
- `meaningful_copy`（warning）：按钮 / 链接文案具体可理解，未命中禁用文案。
