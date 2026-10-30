# Shared Primitives

- 何时使用：文本、颜色、图标、URL、确认弹窗、i18n。
- 主要用途：本文件只做路由；字段定义拆到最小定义粒度。

## JSON 结构示例

```json
{
  "schema": "2.0",
  "config": {
    "compact_width": false
  },
  "body": {
    "elements": [
      {
        "tag": "div",
        "text": {
          "tag": "plain_text",
          "content": "文本示例",
          "text_align": "left"
        },
        "icon": {
          "tag": "standard_icon",
          "token": "info_outlined",
          "color": "blue"
        }
      }
    ]
  }
}
```

## Definitions

| 定义 | 用途 | 读取 |
| --- | --- | --- |
| `BaseText` | 文本对象联合引用，可接受普通文本、Markdown 等文本结构。 | `（未随包提供：base-text）` |
| `PlainText` | 普通文本对象，用于标题、按钮、占位符等不需要 Markdown 的文案。 | `（未随包提供：plain-text）` |
| `LarkMd` | 轻量 Markdown 文本对象，用于兼容 lark_md 文案。 | `（未随包提供：lark-md）` |
| `I18nPlainText` | 多语言普通文本映射。 | `（未随包提供：i18n-plain-text）` |
| `Color` | 官方颜色枚举值和自定义 RGBA 颜色 token 引用。 | `schema/primitive-color.md` |
| `Icon` | 图标联合引用，可接受图标库、自定义图片图标或 UD 图标。 | `schema/primitive-icon.md` |
| `StandardIcon` | 飞书图标库图标配置。 | `schema/primitive-icon.md` |
| `CustomIcon` | 自定义图片图标配置。 | `（未随包提供：custom-icon）` |
| `UDIcon` | UD 图标配置。 | `schema/primitive-icon.md` |
| `UDStyle` | UD 图标颜色、尺寸和内边距样式。 | `（未随包提供：ud-style）` |
| `URL` | 多端跳转 URL 配置。 | `（未随包提供：url）` |
| `Confirm` | 交互前二次确认弹窗配置。 | `（未随包提供：confirm）` |
