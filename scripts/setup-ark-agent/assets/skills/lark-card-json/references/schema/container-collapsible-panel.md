# 折叠面板 Collapsible Panel

- 何时使用：容器 / 折叠面板
- 主要用途：折叠面板用于收纳较长的日志、补充说明或次要详情，让卡片第一屏保持紧凑。

## 组件介绍

折叠面板用于收纳较长的日志、补充说明或次要详情，让卡片第一屏保持紧凑。

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
        "tag": "collapsible_panel",
        "expanded": false,
        "header": {
          "title": {
            "tag": "plain_text",
            "content": "查看详情",
            "text_align": "left"
          },
          "expanded_title": {
            "tag": "plain_text",
            "content": "收起详情",
            "text_align": "left"
          }
        },
        "elements": [
          {
            "tag": "markdown",
            "content": "这里放详细内容",
            "text_align": "left"
          }
        ]
      }
    ]
  }
}
```

## 字段说明

```ts
// import { BaseCollapsiblePanelElement } from '../../body-and-nesting/base-collapsible-panel-element'
// import { Color } from '../../../config/shared-primitives/color'
// import { Icon } from '../../../config/shared-primitives/icon'
// import { LarkMd } from '../../../config/shared-primitives/lark-md'
// import { PlainText } from '../../../config/shared-primitives/plain-text'
// import { Markdown } from '../../../components/content-components/rich-text'

const CollapsiblePanelHeaderTitle = z.union([
  LarkMd.describe('轻量 Markdown 文本对象'),
  Markdown.describe('富文本 Markdown 组件'),
  PlainText.describe('普通文本对象'),
])

const CollapsiblePanelBorder = z.object({
  color: Color.describe('颜色 token 或颜色值').optional(),
  corner_radius: z.string().describe('圆角大小').optional(),
})

const CollapsiblePanelHeader = z.object({
  expanded_title: CollapsiblePanelHeaderTitle.describe('展开状态标题'),
  background_color: Color.describe('背景颜色').optional(),
  icon: Icon.describe('图标对象').optional(),
  icon_expanded_angle: z.union([z.literal(180), z.literal(-180), z.literal(90), z.literal(-90)]).describe('折叠面板展开时图标旋转角度。180=顺时针 180；-180=逆时针 180；90=顺时针 90；-90=逆时针 90').optional(),
  icon_position: z.enum(['right', 'left', 'follow_text']).describe('折叠面板图标位置。right=右侧；left=左侧；follow_text=跟随文本').optional(),
  padding: z.string().describe('内边距').optional(),
  position: z.enum(['top', 'bottom']).describe('位置。top=顶部；bottom=底部').optional(),
  title: CollapsiblePanelHeaderTitle.describe('标题').optional(),
  vertical_align: z.enum(['center', 'top', 'bottom']).describe('垂直对齐方式。center=中；top=上；bottom=下').optional(),
  width: z.enum(['fill', 'auto', 'auto_when_fold']).describe('宽度。fill=填满；auto=自适应；auto_when_fold=折叠时自适应').optional(),
})

const CollapsiblePanel = z.object({
  elements: z.array(BaseCollapsiblePanelElement).describe('子组件数组'),
  header: CollapsiblePanelHeader.describe('标题区或表头配置'),
  tag: z.literal('collapsible_panel').describe('组件标签，固定为 collapsible_panel').optional(),
  background_color: Color.describe('背景颜色').optional(),
  border: CollapsiblePanelBorder.describe('边框配置').optional(),
  direction: z.enum(['vertical', 'horizontal']).describe('子元素排列方向。vertical=垂直；horizontal=水平').optional(),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
  expanded: z.boolean().describe('折叠面板是否默认展开').optional(),
  horizontal_align: z.enum(['left', 'center', 'right']).describe('水平对齐方式。left=左；center=中；right=右').optional(),
  horizontal_spacing: z.string().describe('水平间距').optional(),
  margin: z.string().describe('外边距').optional(),
  padding: z.string().describe('内边距').optional(),
  vertical_align: z.enum(['top', 'center', 'bottom']).describe('垂直对齐方式。top=上；center=中；bottom=下').optional(),
  vertical_spacing: z.string().describe('垂直间距').optional(),
})

const CollapseDivider = z.object({
  tag: z.literal('collapse_divider').describe('组件标签，固定为 collapse_divider').optional(),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
})
```


## 注意事项

- 面板内容放在 `elements` 中。
- 标题区需要配置折叠和展开状态的标题。
- 面板内不适合放需要单独提交的表单。
