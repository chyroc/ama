# 模型厂商图标生成

**读它的时机**：模型清单里加了新厂商、或要换某家的厂商标。

## 一句话

`generate.py` 从上游 `@lobehub/icons-static-svg` 抽出三家模型厂商的品牌图标路径，
产出 `web/src/components/modelIconPaths.ts`。**产物是生成的，别手改**：

```bash
python3 scripts/model-icons/generate.py
```

幂等，同一个上游版本重跑不会有 diff。

## 加一个新厂商

1. 在脚本的 `VENDORS` 里加一行：键是 `MODEL_SPECS` 里的 `vendor` 字段（**逐字一致**，
   `ModelIcon` 靠它查表），值是上游的文件名（不带 `.svg`）；
2. 重跑脚本；
3. `npm run e2e -- -g 厂商标` —— 那条用例按厂商计数，漏了会红。

上游有哪些图标：`npm pack @lobehub/icons-static-svg` 解开看 `icons/`，
或去 <https://icons.lobehub.com/>。

## 为什么内联，而不是装依赖

只用 3 个图标，两个包都不划算：

| 方案 | 问题 |
| --- | --- |
| `@lobehub/icons`（React 组件版） | 拖进 **antd 6 + @lobehub/ui + antd-style** 三个 peer 依赖、9 MB，与本仓库的 HeroUI 3 + Tailwind 4 直接冲突 |
| `@lobehub/icons-static-svg`（纯 SVG） | 干净（MIT、零 peer 依赖）但 2.3 MB 里只要 3 个文件；且 Vite 把 SVG 当资源引入要额外配插件（本仓库没有），`<img src>` 又让图标走一次 HTTP 请求 |

仓库已有内联的先例与理由：`scripts/brand-icons/` + `web/src/components/brandPaths.ts`
（品牌位在首屏，`<img>` 会晚一个 RTT 到）。`web/src/lib/icons.ts` 那套
「图标必须离线可用」的原则指向同一个做法。

## 坑：只处理「朴素 path」，遇到渐变直接报错退出

脚本会检查 `<defs>` / `linearGradient` / `url(#` / `transform=` / `<g `，
命中任一就退出而不是跳过。**因为静默跳过会产出一个少了半张图的图标，
而那种缺陷在 16px 下看不出来。**

上游 `chatglm-color.svg` 就是这种情况：带 `<defs>` 渐变、且 gradient id 是**硬编码**的
（`lobe-icons-chat-glm-_R_0_`），同一页面出现两次会 id 撞车（后一个的定义覆盖前一个）。
智谱那三个模型在方舟叫 `glm-*` 但品牌方是智谱，用 `zhipu-color.svg` 才是厂商标，
正好避开了这个坑——**要是哪天真需要 chatglm，得先给 id 加前缀再内联。**

## 坑：认不出的厂商要「不画」，不是画兜底图

`ModelIcon` 查不到 vendor 时返回 `null`。看着像偷懒，其实是刻意的：
`MODEL_SPECS` 加了新厂商而这里没补图标时，**「少一个图标」比「三家都显示同一个
机器人头」更容易被发现**——后者看起来像正常的，review 时不会停下来。

## 深色模式：对比度是算出来的

非文本图形的可辨识门槛是 3:1（WCAG 1.4.11）。实测：

| 品牌色 | vs 浅色底 `#fff` | vs 深色底 `#161719` |
| --- | --- | --- |
| deepseek `#4D6BFE` | 4.33 | 4.14 |
| zhipu `#3859FF` | 5.20 | 3.45 |
| doubao 主蓝 `#1E37FC` | 6.98 | **2.57** |
| doubao 青 `#37E1BE` | **1.66** | 10.83 |

所以 `fill` 写死十六进制、不跟随主题，也不需要 `dark:` 变体（本仓库全站不用它）。
**doubao 那两行是重点**：它是个多色标，主蓝在深色下只有 2.57、青色在浅色下只有 1.66，
单看任一色都不够——但两色在同一个图形里互补，哪套主题下都至少有一色高对比。
想把它换成单色版之前先想清楚这件事。

## 许可

上游 <https://github.com/lobehub/lobe-icons>，MIT。抽的是 `*-color.svg`（品牌本色版）。
**图标本身是各公司的商标**，这里只作产品内的厂商标识使用。
