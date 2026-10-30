# Body And Nesting

- 何时使用：body 布局和各容器可嵌套集合。
- 主要用途：本文件只做路由；字段定义拆到最小定义粒度。

## JSON 结构示例

```json
{
  "schema": "2.0",
  "config": {
    "compact_width": false
  },
  "body": {
    "direction": "vertical",
    "elements": [
      {
        "tag": "markdown",
        "content": "Body 示例",
        "text_align": "left"
      }
    ],
    "vertical_spacing": "8px"
  }
}
```

## Definitions

| 定义                                | 用途                                        | 读取                                                                                      |
|-----------------------------------|-------------------------------------------|-----------------------------------------------------------------------------------------|
| `CardBody`                        | 卡片 body 容器，组织正文 elements 和布局间距。           | `（未随包提供：card-body）`                          |
| `BaseModule`                      | body.elements 可接受的根级组件集合。                 | `（未随包提供：base-module）`                        |
| `BaseColumnElement`               | column.elements 可接受的子组件集合。                | `（未随包提供：base-column-element）`                |
| `BaseFormElem`                    | form.elements 可接受的表单子组件集合。                | `（未随包提供：base-form-elem）`                     |
| `BaseInteractiveContainerElement` | interactive_container.elements 可接受的子组件集合。 | `（未随包提供：base-interactive-container-element）` |
| `BaseCollapsiblePanelElement`     | collapsible_panel.elements 可接受的子组件集合。     | `（未随包提供：base-collapsible-panel-element）`     |

## 已废弃嵌套结构

- `BaseActionElem` 和 `NoteNode` 已随 `action` / `note` 模块废弃，不再作为可读取定义。交互组合使用根级或容器内的 `button` / `overflow`；备注文本使用 `div`。
