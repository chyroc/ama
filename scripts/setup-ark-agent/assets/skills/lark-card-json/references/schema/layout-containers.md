# 容器组件

- 何时使用：需要查看 容器组件 下的官方组件。
- 主要用途：路由到最小组件粒度 skill。

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
        "tag": "column_set",
        "background_style": "default",
        "flex_mode": "flow",
        "horizontal_spacing": "8px",
        "columns": [
          {
            "tag": "column",
            "width": "weighted",
            "weight": 1,
            "elements": [
              {
                "tag": "markdown",
                "content": "**左列**",
                "text_align": "left"
              }
            ]
          },
          {
            "tag": "column",
            "width": "weighted",
            "weight": 1,
            "elements": [
              {
                "tag": "markdown",
                "content": "**右列**",
                "text_align": "left"
              }
            ]
          }
        ]
      }
    ]
  }
}
```

## Components

| 组件                         | 读取                                                                   |
|----------------------------|----------------------------------------------------------------------|
| 分栏 Column Set              | `schema/container-column-set.md`            |
| 表单容器 Form                  | `（未随包提供：form-container）`        |
| 交互容器 Interactive Container | `（未随包提供：interactive-container）` |
| 折叠面板 Collapsible Panel     | `schema/container-collapsible-panel.md`     |
