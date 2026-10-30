# Card Root

- 何时使用：卡片最外层根对象（schema / config / header / body / 整卡跳转 / 兜底），以及模板卡片载荷。
- 主要用途：本文件用 demo 汇总卡片根的两种形态，命中后只读本文件即可，无需再加载同级子目录。

## 总览

- 卡片根有两种**互斥**形态，二选一：
  - **普通卡片**（`Card`）：直接写 `schema` / `config` / `header` / `body` 等字段。
  - **模板卡片**（`CardJson` + `CardJsonData`）：`type: "template"` + `data` 承载模板 ID 与变量。
- `config` / `header` / `body` / `card_link` 的内部字段不在本文件展开，分别见 `runtime-config/config`、`layout/header/header`、`layout/body-and-nesting/card-body`、`shared-primitives/url`。
- demo 用 `//` 注释标注字段用途与取值；**最终输出必须是合法 JSON**（删注释、去尾逗号），再按本目录的字段定义校验（语法可用 `python3 -m json.tool` 过一遍）。

## 普通卡片（Card）

```json5
{
  "schema": "2.0", // 卡片 JSON 版本，枚举 "1.0" | "2.0"；2.0 结构必须显式声明。必填
  "config": { // 卡片全局配置，结构见 runtime-config/config。可选
    "compact_width": false
  },
  "header": { // 标题区 / 表头，结构见 layout/header/header。可选
    "template": "blue",
    "title": { "tag": "plain_text", "content": "卡片标题", "text_align": "left" }
  },
  "body": { // 正文容器，结构见 layout/body-and-nesting/card-body。可选
    "elements": [
      { "tag": "markdown", "content": "根结构示例", "text_align": "left" }
    ]
  },
  "card_link": { // 整张卡片点击跳转链接（多端 URL 对象，结构见 shared-primitives/url）。可选
    "url": "https://www.feishu.cn"
  },
  "fallback_card": { /* 兜底卡片对象，任意 Card JSON。可选 */ }
}
```

## 模板卡片（CardJson + CardJsonData）

```json5
{
  "type": "template", // 固定值 "template"，标识这是模板卡片封装（CardJson）。必填
  "data": { // 模板卡片数据（CardJsonData）。必填
    "template_id": "ctp_xxxxxxxx",    // 模板 ID。string，可选
    "template_version_name": "1.0.0", // 模板版本名称。string，可选
    "template_variable": {            // 模板变量对象，key→值注入模板。object，可选
      "title": "本月报表"
    },
    "card_id": "xxxxxxxx"             // 卡片 ID。string，可选
  }
}
```

## 注意事项

- 两种形态二选一：直接发整张 `Card`，或发 `{ "type": "template", "data": {...} }` 模板封装。
- `config` / `header` / `body` 等复杂字段此处只给占位，具体字段以各自 skill.md 的 zod 为准。
- 其它定义如需引用根对象，以 `// import { Card } from '../../config/card-root'` 指向本文件，不再指向已合并的子目录。
- demo 里的 `//` 注释仅用于说明；最终输出必须是合法 JSON，按本目录的字段定义校验。
