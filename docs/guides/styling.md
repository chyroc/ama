# 视觉约定

**读它的时机**：改主题、配色、深色模式、圆角。

## 主题

**在 `web/src/index.css`**（简墨：高对比、几乎无彩的浅色，accent 墨蓝近黑），
另有深色变体挂在 `.dark` 上。只覆盖基础 token，其余值由组件样式用
`color-mix` 推导，所以换肤不必碰组件。**覆盖块必须在组件样式之后**才盖得住。

## 可选主题：简墨与澄蓝

「偏好设置 → 外观」里可以在 **简墨（`classic`）** 与 **澄蓝（`azure`）** 之间切换。
简墨就是原来的视觉样式，**它的 token 不要改来迁就另一套**。澄蓝只在
`:root[data-theme="azure"]`（含 `.dark`）里覆盖 token 与局部圆角，采用更柔和的蓝色强调。
主题写在 `<html data-theme>`，与浅深色的 `.dark` 是两个独立维度。
「澄蓝」不仅覆盖 token：`web/src/components/azure/` 里还接入了复制源码的 React 组件
（Button / Chip / SegmentedControl / StatusPill / LoadingState），它们在通用加载、
空状态、错误重试、提示条、偏好分段控件与新建 Agent 按钮里真实渲染。
`classic` 走原组件，`azure` 才渲染这套组件，不在同一个控件上混用两套实现。
组件来源、修改点与 MIT 许可在该目录的 `README.md` 与 `LICENSE` 中，**不要把来源和许可删掉**。
它使用自己的 `@theme inline` 色板映射（`components/azure/theme.css`），只给澄蓝主题提供
实际值；不要引入付费图标包或把参考站点作为运行时依赖。


用户选择存进 `user_preferences.theme` 独立列，前后端各自把未知值归回 `classic`；
缺省与旧行都按简墨。`main.tsx` 在挂载前同时应用浅深色和主题，刷新不会先闪回简墨。
UI 只展示「简墨 / 澄蓝」，不要把内部实现或参考网站的品牌名写进界面。

新增主题或改配色时，要用浏览器验证：点击选择后根节点属性与 accent 真变了，
刷新保持、回切原主题恢复，并看实际截图。`e2e/07-preferences.spec.ts` 的主题用例
用 RGB 比较，别断言浏览器会把 OKLCH 原样输出（它可能输出百分比）。

## 深色不是把浅色明度反过来

三条各自独立的理由：

- **accent 要反转**——浅色下是墨蓝近黑，直接搬到深色底上不可见。
- **分层改用 surface 明度阶梯而非阴影**——暗底上阴影看不见。
- **muted 要够亮**，否则次要文字糊进底色。

改深色前先读 `index.css` 里 `.dark` 那段的注释。

## 图表色板：跑脚本，不要眼估

`--chart-1/2`（categorical，蓝+橙）与 `--chart-ord-1..3`（ordinal，蓝色三档）在
`index.css` 的 `:root` 与 `.dark` 各定义一套。**不能照抄 HeroUI 上游那套
`oklch(from var(--accent) …)` 推导**——本主题 accent 近无彩，推出来是灰阶，
多序列图分不出来。细节见 [heroui.md](heroui.md)。

加/改序列色，用 `dataviz` skill 的 validator 对**本仓库实际的 surface**
（浅色 `#ffffff`、深色 `#1a1c1e`）各跑一遍，别靠看：

```bash
node <dataviz>/scripts/validate_palette.js "#2a78d6,#eb6834" --mode light --surface "#ffffff" --pairs all
node <dataviz>/scripts/validate_palette.js "#86b6ef,#2a78d6,#104281" --mode light --surface "#ffffff" --ordinal
```

两条纪律：**有序的量用单色调阶梯**（P50/P90/平均是同一个量的不同分位，给三个
色相会把「有序」谎报成「三个独立身份」）；**语义色只用于语义**
（`--success`/`--danger` 表示成功失败，不拿来当第三个序列色）。

一条只有跑起来才看得见的：**长尾数据要用对数轴**。耗时分布实测最慢 P90 是最快
P50 的 80 倍，线性刻度下小的那两根压成贴地细线、等于从图上消失。用了 log 就要在
标题下标明，且 `domain` 下界给 1 不给 0（`log(0)` 是负无穷，整张图会渲染成空白）。

## 圆角按元素尺寸分四档

别直接写 `rounded-xl` / `rounded-2xl`——那两个跟着 `--radius` 走，在小元素上会失控
（曾经 32px 圆角配 36px 图标方块，几乎成圆形）：

| 类 | 半径 | 用于 |
| --- | --- | --- |
| `r-container` | 12px | 卡片、模态、空态框 |
| `r-row` | 10px | 可点击行、导航项 |
| `r-inset` | 8px | 代码块、提示条、预览块 |
| `r-chip` | 6px | 图标底衬等小方块 |

`rounded-full` 只留给头像和时间线圆点（刻意的圆）。

HeroUI 自己把 card/chip/button 的圆角写得偏大（chip 是 16px 而它只有 20px 高，
等于胶囊），`index.css` 的 `@layer components` 里统一压过了——**改圆角先看那一段**。
