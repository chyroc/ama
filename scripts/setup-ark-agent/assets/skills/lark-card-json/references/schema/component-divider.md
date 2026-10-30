# 分割线 Divider

- 何时使用：展示 / 分割线
- 主要用途：分割线用于分隔卡片内容区块，帮助建立信息层级。

## 组件介绍

分割线用于分隔卡片内容区块，帮助建立信息层级。

## 字段说明

```ts
const Hr = z.object({
  tag: z.literal('hr').describe('组件标签，固定为 hr').optional(),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
  margin: z.string().describe('外边距').optional(),
})
```


## 注意事项

- 只需要视觉分隔时优先用分割线，不要伪造空交互容器。
- 分割线适合放在同级内容块之间。
