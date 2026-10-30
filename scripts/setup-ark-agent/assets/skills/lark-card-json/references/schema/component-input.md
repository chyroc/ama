# 输入框 Input

- 何时使用：交互 / 输入框
- 主要用途：输入框用于收集单行、多行或密码类型文本，可放入表单后随提交按钮一起回传。

## 组件介绍

输入框用于收集单行、多行或密码类型文本，可放入表单后随提交按钮一起回传。

## 字段说明

```ts
// import { BaseBehavior } from '../../../config/behaviors'
// import { Confirm } from '../../../config/shared-primitives/confirm'
// import { PlainText } from '../../../config/shared-primitives/plain-text'
// import { BaseText } from '../../../config/shared-primitives/base-text'

const Input = z.object({
  tag: z.literal('input').describe('组件标签，固定为 input').optional(),
  auto_resize: z.boolean().describe('输入框是否随内容自动调整高度').optional(),
  behaviors: z.array(BaseBehavior).describe('点击或选择后的交互行为数组').optional(),
  confirm: Confirm.describe('二次确认弹窗配置').optional(),
  custom_action_id: z.string().describe('自定义动作 ID，用于识别交互来源').optional(),
  default_value: z.string().describe('输入框默认值').optional(),
  disabled: z.boolean().describe('是否禁用组件').optional(),
  disabled_tips: PlainText.describe('禁用时提示文案').optional(),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
  input_type: z.enum(['text', 'multiline_text', 'password']).describe('输入框类型。text=单行文本;multiline_text=多行文本;password=密码').optional(),
  label: BaseText.describe('输入框标签').optional(),
  label_position: z.enum(['top', 'left']).describe('输入框标签位置。top=上方;left=左侧').optional(),
  lambda: z.string().describe('动态表达式或低代码动作字段；普通 JSON 卡片通常不填').optional(),
  margin: z.string().describe('外边距').optional(),
  max_length: z.number().describe('输入最大长度').optional(),
  max_rows: z.number().describe('多行输入最大行数').optional(),
  name: z.string().describe('交互或表单字段名称，用于回传识别').optional(),
  placeholder: PlainText.describe('占位提示文案').optional(),
  required: z.boolean().describe('表单项是否必填').optional(),
  rows: z.number().describe('表格行数据').optional(),
  show_icon: z.boolean().describe('是否展示输入图标').optional(),
  tmp_action_id: z.string().describe('临时动作 ID，用于运行时识别交互').optional(),
  value: z.record(z.unknown()).describe('回传值').optional(),
  webhook: z.string().describe('交互回调 webhook').optional(),
  width: z.string().describe('宽度').optional(),
})
```


## 注意事项

- 表单内输入项应设置唯一 `name`。
- 多行输入可配置 `rows`、`max_rows`、`auto_resize`。
- 敏感输入可使用 `password` 类型。
