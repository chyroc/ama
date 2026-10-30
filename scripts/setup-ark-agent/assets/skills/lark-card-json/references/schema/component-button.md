# 按钮 Button

- 何时使用：交互 / 按钮
- 主要用途：按钮用于触发打开链接、回调、表单提交或重置等用户操作。

## 组件介绍

按钮用于触发打开链接、回调、表单提交或重置等用户操作。

## 字段说明

```ts
// import { BaseBehavior } from '../../../config/behaviors'
// import { Confirm } from '../../../config/shared-primitives/confirm'
// import { PlainText } from '../../../config/shared-primitives/plain-text'
// import { BaseText } from '../../../config/shared-primitives/base-text'
// import { Icon } from '../../../config/shared-primitives/icon'
// import { URL } from '../../../config/shared-primitives/url'

const Button = z.object({
  type: z.enum(['default', 'primary', 'danger', 'link', 'laser', 'primary_filled', 'danger_filled', 'primary_text', 'danger_text', 'text']).describe('类型。default=默认；primary=主按钮；danger=危险；link=链接；laser=激光；primary_filled=填充主按钮；danger_filled=填充危险按钮；primary_text=文本主按钮；danger_text=文本危险按钮；text=文本'),
  tag: z.literal('button').describe('组件标签，固定为 button').optional(),
  action_type: z.enum(['link', 'request', 'multi', 'form_submit', 'form_reset']).describe('动作类型。link=跳转；request=请求；multi=多端；form_submit=提交；form_reset=重置').optional(),
  behaviors: z.array(BaseBehavior).describe('点击或选择后的交互行为数组').optional(),
  confirm: Confirm.describe('二次确认弹窗配置').optional(),
  custom_action_id: z.string().describe('自定义动作 ID，用于识别交互来源').optional(),
  disabled: z.boolean().describe('是否禁用组件').optional(),
  disabled_tips: PlainText.describe('禁用时提示文案').optional(),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
  form_action_type: z.enum(['submit', 'reset']).describe('表单按钮动作类型。submit=提交；reset=重置').optional(),
  form_name: z.string().describe('关联表单名称').optional(),
  hover_tips: PlainText.describe('鼠标悬浮提示').optional(),
  icon: Icon.describe('图标对象').optional(),
  lambda: z.string().describe('动态表达式或低代码动作字段；普通 JSON 卡片通常不填').optional(),
  margin: z.string().describe('外边距').optional(),
  multi_url: URL.describe('多端 URL 对象').optional(),
  name: z.string().describe('交互或表单字段名称，用于回传识别').optional(),
  required: z.boolean().describe('表单项是否必填').optional(),
  size: z.enum(['small', 'medium', 'large', 'tiny']).describe('尺寸。small=小；medium=中；large=大；tiny=极小').optional(),
  text: BaseText.describe('文本对象').optional(),
  tmp_action_id: z.string().describe('临时动作 ID，用于运行时识别交互').optional(),
  url: z.string().describe('默认 URL').optional(),
  value: z.record(z.unknown()).describe('回传值').optional(),
  webhook: z.string().describe('交互回调 webhook').optional(),
  width: z.string().describe('宽度').optional(),
})
```


## 注意事项

- 按钮应有明确动作，不要做装饰性假按钮。
- 回调按钮使用对象类型 `value`。
- 表单按钮使用 `form_action_type` 绑定提交或重置。
- 替代交互模块时直接把 `button` 放入 `body.elements`、`column.elements`、`form.elements` 或其它支持的容器；通过父容器 `vertical_spacing` / `horizontal_spacing` 控制间距，不要生成 `{"tag":"action"}`。
