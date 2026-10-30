# 图表 Chart

- 何时使用：展示 / 图表
- 主要用途：图表组件基于 VChart 配置展示折线图、柱状图、饼图、进度图等数据可视化。

## 组件介绍

图表组件基于 VChart 配置展示折线图、柱状图、饼图、进度图等数据可视化。

## 字段说明

```ts
const Chart = z.object({
  chart_spec: z.record(z.unknown()).describe('VChart 的 JSON 配置对象'),
  color_theme: z.enum(['brand', 'rainbow', 'complementary', 'converse', 'primary']).describe('图表配色主题。brand=品牌；rainbow=彩虹；complementary=互补；converse=反向；primary=主色'),
  tag: z.literal('chart').describe('组件标签，固定为 chart').optional(),
  aspect_ratio: z.enum(['16:9', '4:3', '2:1', '1:1']).describe('图表或图片宽高比。16:9=宽屏；4:3=传统；2:1=横向；1:1=正方形').optional(),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
  height: z.string().describe('高度').optional(),
  margin: z.string().describe('外边距').optional(),
  preview: z.boolean().describe('是否允许点击预览').optional(),
})
```


## 注意事项

- `chart_spec` 必须是纯 JSON 对象，不能包含函数或 JavaScript 表达式。
- 数据优先使用扁平对象数组。
- 图表类型和字段名应与 VChart 配置保持一致。
