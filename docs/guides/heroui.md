# HeroUI v3 踩过的坑

**读它的时机**：用到 HeroUI 组件时。这几个都是「看着对、跑起来坏」的，**改前先读**。

装包与 token 问题见 [environment.md](environment.md)，配色圆角见 [styling.md](styling.md)。

## OSS（`@heroui/react`）

**Modal 必须 `Modal.Backdrop > Modal.Container` 嵌套。** `Backdrop` 是 RAC 的
`ModalOverlay`、`Container` 是 `Modal`，写成兄弟节点时 RAC 收不到退出动画的
`animationend`，backdrop 会永久停在 `data-exiting` 状态、**挡住整页点击**。
受控用法把 `isOpen`/`onOpenChange` 给 `Modal.Backdrop`，不是给 `Modal`。
统一走 `src/components/FormModal.tsx`，别各页自己拼。

**`Dropdown.Trigger` 自己就是 `<button>`。** 再往里套 `<Button>` 会得到嵌套 button，
React 直接报 DOM 校验错误。直接给 Trigger 上 className。
（`Tooltip.Trigger` 不同，它是 Focusable 包装，里面放 Button 是对的。）

**`Select` 的 `selectedKey` 指向集合里没有的 item 时，`Select.Value` 渲染成空白。**
不报错、不占位、不回落到 placeholder。踩到它的场景是「agent 被删掉的已有会话」——
`selectedKey` 还是那个已删的 id，于是选择器空着，界面上表现成**这条会话不属于任何人**。
兜底走 `Select.Value` 的 render function（官方 "Custom Value" 那套，render props 是
`{defaultChildren, isPlaceholder, state}`）：

```tsx
<Select.Value>
  {({ defaultChildren, state }) =>
    state.selectedItems.length === 0 && fallbackName ? fallbackName : defaultChildren}
</Select.Value>
```

**`Dropdown.Section` 没有 `title` 属性**，传了会编译报错。

**Tailwind 的 `line-clamp-*` 在这里不生效**，得用 `src/index.css` 里的 `.clamp-2`。
另外裁行发生在 padding 边缘，所以别在带 `py-*` 的元素上裁行——多出来的那行会漏进
下内边距，看着像渲染坏了。padding 放外层容器。

本仓库只用免费的 `@heroui/react`。Pro 组件（`@heroui-pro/react`）是付费包，**不要引入**；
需要的复合组件（Segment / Stepper / Timeline / Chat 等）在 `web/src/components/ui/` 下自己实现。
