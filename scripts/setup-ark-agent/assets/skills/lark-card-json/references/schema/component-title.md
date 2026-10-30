# 标题 Header

- 何时使用：展示 / 标题
- 主要用途：标题组件展示卡片主标题、副标题、状态标签和前缀图标，是卡片顶部的信息入口。

## 组件介绍

标题组件展示卡片主标题、副标题、状态标签和前缀图标，是卡片顶部的信息入口。

## 字段说明

```ts
// import { BaseText } from '../../../config/shared-primitives/base-text'
// import { PlainText } from '../../../config/shared-primitives/plain-text'
// import { Icon } from '../../../config/shared-primitives/icon'

const TextTag = z.object({
  color: z.string().describe('颜色 token 或颜色值'),
  text: PlainText.describe('文本对象'),
  tag: z.literal('text_tag').describe('组件标签，固定为 text_tag').optional(),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
})

const Header = z.object({
  title: BaseText.describe('标题'),
  direction: z.enum(['vertical', 'horizontal']).describe('子元素排列方向。vertical=垂直；horizontal=水平').optional(),
  horizontal_align: z.enum(['left', 'center', 'right']).describe('水平对齐方式。left=左；center=中；right=右').optional(),
  horizontal_spacing: z.string().describe('水平间距').optional(),
  icon: Icon.describe('图标对象').optional(),
  margin: z.string().describe('外边距').optional(),
  padding: z.string().describe('内边距').optional(),
  subtitle: BaseText.describe('标题副标题').optional(),
  template: z.string().describe('标题模板颜色').optional(),
  text_tag_list: z.array(TextTag).describe('标题右侧或标题区标签列表').optional(),
  vertical_align: z.enum(['top', 'center', 'bottom']).describe('垂直对齐方式。top=上；center=中；bottom=下').optional(),
  vertical_spacing: z.string().describe('垂直间距').optional(),
})
```


## 注意事项

- 一张卡片通常只设置一个根级 `header`。
- 标题应简短，状态信息可用 `text_tag_list` 辅助表达。
