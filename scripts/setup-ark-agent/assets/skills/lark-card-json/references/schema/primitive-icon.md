# Icon

- 何时使用：当字段类型引用 `Icon`、`StandardIcon` 或 `UDIcon` 时。
- 主要用途：图标统一定义。`Icon` 为图标联合引用（图标库图标或自定义图片图标）；`StandardIcon` 为飞书图标库图标配置（`standard_icon`）；`UDIcon` 为 UD 图标配置（`ud_icon`）。

## 字段说明

```ts
// import { Color } from '../color'
// import { CustomIcon } from '../custom-icon'
// import { UDStyle } from '../ud-style'

const StandardIcon = z.object({
  tag: z.literal('standard_icon').describe('组件标签，固定为 standard_icon').optional(),
  color: Color.describe('颜色 token 或颜色值'),
  token: z.string().describe('图标 token'),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
  horizontal_padding: z.number().describe('图标水平内边距').optional(),
  margin: z.string().describe('外边距').optional(),
  size: z.string().describe('尺寸').optional(),
})

const UDIcon = z.object({
  tag: z.literal('ud_icon').describe('组件标签，固定为 ud_icon').optional(),
  style: UDStyle.describe('组件展示样式'),
  token: z.string().describe('图标 token'),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
  margin: z.string().describe('外边距').optional(),
})

const Icon = z.union([
  StandardIcon.describe('飞书图标库图标'),
  CustomIcon.describe('自定义图片图标'),
])
```

## 图标枚举数据

- 全量目录与分类统计：`（未随包提供：index.json）`
- 按风格和分类拆分的图标列表：`（未随包提供：<category>.json）`

## 注意事项

- 字段定义以本文件的 zod schema 为准；输出后使用校验脚本验证。
- 只需要本定义时不要加载同级其它组件文件。
