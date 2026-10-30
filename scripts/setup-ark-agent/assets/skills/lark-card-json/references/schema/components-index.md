# 组件总览

- 何时使用：需要查看 Card JSON 2.0 全部 展示组件 / 交互组件 的官方组件清单。
- 主要用途：路由到最小组件粒度 skill，并给出每个组件的核心用途。

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
        "tag": "markdown",
        "content": "**正文内容**",
        "text_align": "left"
      },
      {
        "tag": "button",
        "type": "primary",
        "text": {
          "tag": "plain_text",
          "content": "查看详情",
          "text_align": "left"
        },
        "behaviors": [
          {
            "type": "open_url",
            "default_url": "https://example.com"
          }
        ]
      }
    ]
  }
}
```

## 展示组件 Content Components

| 组件               | 用途                                              | 读取                                                                            |
|------------------|-------------------------------------------------|-------------------------------------------------------------------------------|
| 标题 Header        | 展示卡片主标题、副标题、状态标签和前缀图标，是卡片顶部的信息入口。               | `schema/component-title.md`              |
| 普通文本 Div         | 展示短文本、字段信息和可控样式文本，也可以带图标或右侧附加组件。                | `schema/component-plain-text.md`         |
| 富文本 Markdown     | 展示正文、列表、强调、链接、代码块、引用和较复杂的 Markdown 内容。          | `schema/component-rich-text.md`          |
| 图片 Image         | 通过 `img_key` 展示单张图片，可设置标题、替代文本、圆角、裁剪方式、大小和预览行为。 | `schema/component-image.md`              |
| 多图混排 Multi Image | 以双图、三图或宫格方式展示多张图片。                              | `（未随包提供：multi-image-layout）` |
| 人员 Person        | 展示单个用户的头像、姓名和个人名片入口。                            | `（未随包提供：user-profile）`       |
| 人员列表 Person List | 展示多个人员头像或姓名，适合负责人、参与人、审批人等场景。                   | `（未随包提供：user-list）`          |
| 图表 Chart         | 基于 VChart 配置展示折线图、柱状图、饼图、进度图等数据可视化。             | `schema/component-chart.md`              |
| 表格 Table         | 展示结构化数据，支持文本、数字、选项、人员、日期和 Markdown 等列类型。        | `schema/component-table.md`              |
| 音频 Audio         | 在卡片中展示可播放的音频文件，可控制背景、进度条、时间显示和边框。               | `（未随包提供：audio）`              |
| 分割线 Divider      | 分隔卡片内容区块，帮助建立信息层级。                              | `schema/component-divider.md`            |

### 兼容/辅助展示组件

| 组件                     | 用途                                                | 读取                                                                       |
|------------------------|---------------------------------------------------|--------------------------------------------------------------------------|
| 头像 Avatar（兼容）          | 展示单个用户头像；新卡片中需要头像加姓名时优先使用人员组件或人员列表。               | `（未随包提供：avatar）`        |
| 视频 Video（兼容）           | 在卡片中展示可播放的视频资源，可配置封面、下载和时长展示。                     | `（未随包提供：video）`         |
| 兜底文本 Fallback Text（兼容） | 在特定能力不可用或降级场景展示替代文本，也可带交互行为。                      | `（未随包提供：fallback-text）` |

## 交互组件 Interactive Components

| 组件                            | 用途                                              | 读取                                                                                         |
|-------------------------------|-------------------------------------------------|--------------------------------------------------------------------------------------------|
| 输入框 Input                     | 收集单行、多行或密码类型文本，可放入表单后随提交按钮一起回传。                 | `schema/component-input.md`                       |
| 按钮 Button                     | 触发打开链接、回调、表单提交或重置等用户操作。                         | `schema/component-button.md`                      |
| 折叠按钮组 Overflow                | 收纳多个次要操作，减少卡片操作区占用空间。                           | `（未随包提供：overflow）`                    |
| 下拉选择-单选 Select Static         | 从固定选项中选择一个值。                                    | `（未随包提供：single-select-dropdown-menu）` |
| 下拉选择-多选 Multi Select Static   | 从固定选项中选择多个值。                                    | `（未随包提供：multi-select-dropdown-menu）`  |
| 人员选择-单选 Select Person         | 让用户选择一个人员，常见于负责人、审批人、处理人选择。                     | `（未随包提供：single-select-user-picker）`   |
| 人员选择-多选 Multi Select Person   | 让用户选择多个成员，常见于参与人、抄送人或协作人列表。                     | `（未随包提供：multi-select-user-picker）`    |
| 日期选择器 Date Picker             | 选择年月日日期。                                        | `（未随包提供：date-picker）`                 |
| 时间选择器 Time Picker             | 选择一天内的具体时间。                                     | `（未随包提供：time-selector）`               |
| 日期时间选择器 DateTime Picker       | 同时选择日期和时间。                                      | `（未随包提供：date-time-picker）`            |
| 多图选择 Image Picker             | 从多张图片中选择一张或多张图片。                                | `（未随包提供：image-picker）`                |
| 勾选器 Checker                   | 表达待办项、确认项或可切换状态。                                | `（未随包提供：checker）`                     |

### 兼容/辅助交互结构

| 组件                           | 用途                                           | 读取                                                                                           |
|------------------------------|----------------------------------------------|----------------------------------------------------------------------------------------------|
| 日期时间选择共享结构 BaseDatePicker    | 日期、时间、日期时间选择器的共享引用集合，用于判断容器或字段可接受哪些日期时间选择组件。 | `（未随包提供：base-date-picker）`       |
| 单选选择共享结构 BaseSelectMenu      | 人员单选和静态选项单选的共享引用集合。                          | `（未随包提供：base-select-menu）`       |
| 多选选择共享结构 BaseMultiSelectMenu | 人员多选和静态选项多选的共享引用集合。                          | `（未随包提供：base-multi-select-menu）` |

## 已废弃组件

- Card JSON 2.0 不再支持备注组件 `{"tag":"note"}`。备注、脚注、低权重提示改读 `schema/component-plain-text.md`，使用 `div` 配置 `text_size: "notation"`、`text.text_color: "grey"` 和 `icon`/`ud_icon`。
- Card JSON 2.0 不再支持交互模块 `{"tag":"action"}`。单个或少量操作改读 `schema/component-button.md`；多个次要操作改读 `（未随包提供：overflow）`。操作间距通过父容器或分栏的 `vertical_spacing` / `horizontal_spacing` 控制。

## 使用规则

- 先读本入口；再读命中的组件 `skill.md`，不再读取 `content-components/` 或 `interactive-components/` 下的中间 `skill.md`。
- 组件 skill 包含组件介绍、字段说明、注意事项。
- 字段说明统一使用 TypeScript zod schema 代码块，其中 `.describe()` 承载字段含义、`.optional()` 标记可选、`z.enum([...]).describe('...')` 写枚举值含义、顶部用 `// import { X } from '<相对路径>'` 注释引用跨文件类型；不要在其它 reference 文件维护第二套字段定义。
- 官方菜单内组件按展示、交互粒度区分；schema 中存在但不在菜单内的兼容组件或共享结构，收敛在对应分类的“兼容/辅助”小节。
