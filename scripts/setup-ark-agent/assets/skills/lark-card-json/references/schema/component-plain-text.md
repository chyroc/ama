# 普通文本 Div

- 何时使用：展示 / 普通文本
- 主要用途：普通文本组件用于展示短文本、字段信息和可控样式文本，也可以带图标或右侧附加组件。

## 组件介绍

普通文本组件用于展示短文本、字段信息和可控样式文本，也可以带图标或右侧附加组件。

## 字段说明

```ts
// import { BaseText } from '../../../config/shared-primitives/base-text'
// import { Icon } from '../../../config/shared-primitives/icon'
// import { UDIcon } from '../../../config/shared-primitives/icon'
// import { DatePicker } from '../../interactive-components/date-picker'
// import { PickerDate } from '../../interactive-components/date-picker'
// import { PickerDatetime } from '../../interactive-components/date-time-picker'
// import { PickerTime } from '../../interactive-components/time-selector'
// import { MultiSelectPerson } from '../../interactive-components/multi-select-user-picker'
// import { MultiSelectStatic } from '../../interactive-components/multi-select-dropdown-menu'
// import { SelectPerson } from '../../interactive-components/single-select-user-picker'
// import { SelectStatic } from '../../interactive-components/single-select-dropdown-menu'
// import { Button } from '../../interactive-components/button'
// import { Checker } from '../../interactive-components/checker'
// import { FallbackText } from '../fallback-text'
// import { Input } from '../../interactive-components/input'
// import { Overflow } from '../../interactive-components/overflow'
// import { SelectImg } from '../../interactive-components/image-picker'
// import { Img } from '../image'

const DivExtra = z.union([
  DatePicker.describe('旧版日期选择器结构'),
  PickerDate.describe('日期选择器'),
  PickerDatetime.describe('日期时间选择器'),
  PickerTime.describe('时间选择器'),
  MultiSelectPerson.describe('人员多选组件'),
  MultiSelectStatic.describe('静态选项多选组件'),
  SelectPerson.describe('人员单选组件'),
  SelectStatic.describe('静态选项单选组件'),
  Button.describe('按钮组件'),
  Checker.describe('勾选器组件'),
  FallbackText.describe('降级或兜底文本组件'),
  Input.describe('输入框组件'),
  Overflow.describe('折叠按钮组'),
  SelectImg.describe('图片选择组件'),
  Img.describe('图片组件'),
])

const Field = z.object({
  is_short: z.boolean().describe('字段是否短展示'),
  text: BaseText.describe('文本对象'),
})

const Div = z.object({
  text: BaseText.describe('文本对象'),
  tag: z.literal('div').describe('组件标签，固定为 div').optional(),
  element_id: z.string().describe('组件唯一标识，用于组件 API、交互定位和更新').optional(),
  extra: DivExtra.describe('div 右侧附加组件').optional(),
  fields: z.array(Field).describe('div 字段区，适合左右成组展示短信息').optional(),
  icon: Icon.describe('图标对象').optional(),
  margin: z.string().describe('外边距').optional(),
  text_size: z.enum(['normal', 'heading', 'notation']).describe('文本字号。normal=普通；heading=标题；notation=备注').optional(),
  ud_icon: UDIcon.describe('UD 图标对象').optional(),
  width: z.string().describe('宽度').optional(),
})
```


## 注意事项

- 复杂段落和列表优先使用富文本 Markdown。
- 需要两列字段时使用 `fields`。
- 右侧按钮、图片或选择器使用 `extra`。
- 替代备注组件时使用 `div`，配置 `text_size: "notation"`、`text.text_color: "grey"`，并按需要配置 `icon` 或 `ud_icon`；不要生成 `{"tag":"note"}`。
