# 表格 Table

- 何时使用：展示 / 表格
- 主要用途：表格用于展示结构化数据，支持文本、数字、选项、人员、日期和 Markdown 等列类型。

## 组件介绍

表格用于展示结构化数据，支持文本、数字、选项、人员、日期和 Markdown 等列类型。

## 字段说明

```ts
// import { Color } from '../../../config/shared-primitives/color'

const NumberFormat = z.object({
  precision: z.number().describe('数字小数位数').optional(),
  separator: z.boolean().describe('数字是否使用千分位分隔').optional(),
  symbol: z.string().describe('数字前后缀符号').optional(),
})

const HeaderStyle = z.object({
  background_style: z.string().describe('背景样式或颜色 token').optional(),
  bold: z.boolean().describe('是否加粗').optional(),
  lines: z.number().describe('最大展示行数').optional(),
  text_align: z.enum(['left', 'center', 'right']).describe('文本对齐方式。left=左；center=中；right=右').optional(),
  text_color: Color.describe('文本颜色').optional(),
  text_size: z.string().describe('文本字号').optional(),
})

const TableColumn = z.object({
  name: z.string().describe('交互或表单字段名称，用于回传识别'),
  data_type: z.enum(['text', 'options', 'number', 'persons', 'date', 'lark_md', 'markdown']).describe('表格列数据类型。text=文本；options=选项；number=数字；persons=人员；date=日期；lark_md=轻量 Markdown；markdown=完整 Markdown').optional(),
  date_format: z.string().describe('日期展示格式').optional(),
  display_name: z.string().describe('展示名称').optional(),
  format: NumberFormat.describe('数字格式配置').optional(),
  horizontal_align: z.enum(['left', 'center', 'right']).describe('水平对齐方式。left=左；center=中；right=右').optional(),
  vertical_align: z.enum(['top', 'center', 'bottom']).describe('垂直对齐方式。top=上；center=中；bottom=下').optional(),
  width: z.string().describe('宽度').optional(),
})

const Table = z.object({
  columns: z.array(TableColumn).describe('表格列或分栏列数组'),
  rows: z.array(z.record(z.unknown())).describe('表格行数据'),
  tag: z.literal('table').describe('组件标签，固定为 table').optional(),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
  freeze_first_column: z.boolean().describe('是否冻结表格首列').optional(),
  header_style: HeaderStyle.describe('表格表头样式').optional(),
  margin: z.string().describe('外边距').optional(),
  page_size: z.number().describe('表格每页展示行数').optional(),
  row_height: z.string().describe('表格行高').optional(),
  row_max_height: z.string().describe('自动行高时的最大行高').optional(),
  table_key: z.string().describe('表格 key').optional(),
})
```


## 注意事项

- 单张卡片的表格数量应控制，避免首屏过重。
- 列 `name` 需要和行数据 key 对齐。
- `rows` 必须是行对象数组；每个行对象的 key 需要和列 `name` 对齐。
