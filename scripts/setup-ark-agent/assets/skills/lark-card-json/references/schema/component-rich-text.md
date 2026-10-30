# 富文本 Markdown

- 何时使用：展示 / 富文本（Markdown）
- 主要用途：富文本组件用于展示正文、列表、强调、链接、代码块、引用和较复杂的 Markdown 内容。

## 组件介绍

富文本组件用于展示正文、列表、强调、链接、代码块、引用和较复杂的 Markdown 内容。

## 字段说明

```ts
// import { Icon } from '../../../config/shared-primitives/icon'
// import { UDIcon } from '../../../config/shared-primitives/icon'

const Markdown = z.object({
  content: z.string().describe('文本内容'),
  text_align: z.enum(['left', 'right', 'center']).describe('文本对齐方式。left=左；right=右；center=中'),
  tag: z.literal('markdown').describe('组件标签，固定为 markdown').optional(),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
  enable_streaming: z.boolean().describe('文本是否支持流式更新').optional(),
  icon: Icon.describe('图标对象').optional(),
  margin: z.string().describe('外边距').optional(),
  text_lines: z.number().describe('文本最大行数').optional(),
  text_size: z.string().describe('文本字号').optional(),
  ud_icon: UDIcon.describe('UD 图标对象').optional(),
})
```


## 注意事项

- JSON 字符串中的换行写成 `\n`。
- 链接使用 HTTP/HTTPS。
- 需要精确列宽和数据类型时使用表格组件，不要用 Markdown 表格替代。
