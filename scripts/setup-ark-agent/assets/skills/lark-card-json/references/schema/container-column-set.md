# 分栏 Column Set

- 何时使用：容器 / 分栏
- 主要用途：分栏用于在卡片中横向组织多列内容，适合指标对比、图文列表、按钮并排和响应式信息块。

## 组件介绍

分栏用于在卡片中横向组织多列内容，适合指标对比、图文列表、按钮并排和响应式信息块。

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
        "tag": "column_set",
        "background_style": "default",
        "flex_mode": "flow",
        "horizontal_spacing": "8px",
        "columns": [
          {
            "tag": "column",
            "width": "weighted",
            "weight": 1,
            "elements": [
              {
                "tag": "markdown",
                "content": "**左列**",
                "text_align": "left"
              }
            ]
          },
          {
            "tag": "column",
            "width": "weighted",
            "weight": 1,
            "elements": [
              {
                "tag": "markdown",
                "content": "**右列**",
                "text_align": "left"
              }
            ]
          }
        ]
      }
    ]
  }
}
```

## 字段说明

```ts
// import { BaseColumnElement } from '../../body-and-nesting/base-column-element'
// import { Color } from '../../../config/shared-primitives/color'
// import { URL } from '../../../config/shared-primitives/url'

const ColumnSetAction = z.object({
  multi_url: URL.describe('多端 URL 对象'),
})

const ColumnAction = z.object({
  multi_url: URL.describe('多端 URL 对象'),
})

const Column = z.object({
  weight: z.enum([1, 2, 3, 4, 5]).describe('列宽权重，number类型。1=最小；2=较小；3=中；4=较大；5=最大'),
  width: z.string().describe('宽度'),
  tag: z.literal('column').describe('组件标签，固定为 column').optional(),
  action: ColumnAction.describe('点击行为配置').optional(),
  background_style: Color.describe('背景样式或颜色 token').optional(),
  direction: z.enum(['vertical', 'horizontal']).describe('子元素排列方向。vertical=垂直；horizontal=水平').optional(),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
  elements: z.array(BaseColumnElement).describe('子组件数组').optional(),
  horizontal_align: z.enum(['left', 'center', 'right']).describe('水平对齐方式。left=左；center=中；right=右').optional(),
  horizontal_spacing: z.string().describe('水平间距').optional(),
  margin: z.string().describe('外边距').optional(),
  padding: z.string().describe('内边距').optional(),
  vertical_align: z.enum(['top', 'center', 'bottom']).describe('垂直对齐方式。top=上；center=中；bottom=下').optional(),
  vertical_spacing: z.string().describe('垂直间距').optional(),
})

const ColumnSet = z.object({
  background_style: Color.describe('背景样式或颜色 token'),
  flex_mode: z.enum(['none', 'flow', 'stretch', 'bisect', 'trisect']).describe('分栏在窄屏下的响应式模式。none=不换行；flow=流式；stretch=堆叠；bisect=两列；trisect=三列'),
  horizontal_spacing: z.string().describe('水平间距'),
  tag: z.literal('column_set').describe('组件标签，固定为 column_set').optional(),
  action: ColumnSetAction.describe('点击行为配置').optional(),
  columns: z.array(Column).describe('表格列或分栏列数组').optional(),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
  horizontal_align: z.enum(['left', 'center', 'right']).describe('水平对齐方式。left=左；center=中；right=右').optional(),
  margin: z.string().describe('外边距').optional(),
  vertical_align: z.enum(['top', 'center', 'bottom']).describe('垂直对齐方式。top=上；center=中；bottom=下').optional(),
})
```


## 注意事项

- 列内容放在 `columns[].elements` 中。
- 需要窄屏换行时优先使用 `flex_mode: flow` 或等分模式。
- 列或分栏整体点击时使用 action.multi_url，并避免与内部按钮交互冲突。
