# 品牌图标生成

**读它的时机**：设计给了新 logo、或要改品牌位的圆角/描边。

## 一句话

`generate.py` 把设计给的一张位图追成矢量，产出 `web/public/` 下的图标文件与
`web/src/components/brandPaths.ts` 里那三条路径。**产物都是生成的，别手改**：

```bash
python3 scripts/brand-icons/generate.py [源图路径]   # 默认读 ~/tmp/ama.png
```

幂等，重跑不会有 diff。

## 产物与各自的去处

| 文件 | 谁在用 | 少了会怎样 |
| --- | --- | --- |
| `favicon.svg` | `index.html` 的 `rel=icon`，一份矢量管所有尺寸 | 标签页图标空白 |
| `favicon.ico` | 同上，兜底老浏览器与只抓根目录的爬虫（含 16/32/48） | 同上，且部分聚合器抓不到 |
| `apple-touch-icon.png` | iOS「添加到主屏幕」（180×180） | 主屏幕上是一张网页截图 |
| `icon-192/512.png` | `manifest.json` 里列的 PWA 图标 | **没人发现**——只在添加到主屏幕时用到 |
| `manifest.json` | `index.html` 的 `rel=manifest` | 同上 |
| `src/components/brandPaths.ts` | `BrandMark.tsx`（站内两处品牌位） | 编译报错，这个反而是最安全的 |

站内品牌位走**内联 SVG** 而不是 `<img src="/favicon.svg">`：`public/` 下的东西要
发 HTTP 请求，而品牌位在首屏，用 `<img>` 会让 logo 比骨架晚一个 RTT 到，
表现成首屏闪一块空白。理由写在 `BrandMark.tsx` 的文件头。

## 四条踩过的坑

**源图无 alpha，深紫底是实心像素，不要抠。** `ama.png` 的 mode 是 RGB，
四个角也是深紫——它就是一张满幅方图。抠背景一是抠不干净（AI 生成的图边缘有过渡
像素，抠完是毛边），二是 favicon 在 16px 下本来就靠底色才看得清轮廓。

**圆角不写进 SVG。** 源图没有圆角，圆角是显示层的事：`BrandMark` 用仓库既有的
`r-chip`（6px，见 `docs/guides/styling.md` 的四档）。写进 SVG 就会有「SVG 里一个值、
CSS 里另一个」两套，改刻度时必然漏一个。PWA 的 maskable 图标还要求满幅——
平台自己按形状裁，我们先倒一次角会得到双层圆角。

**`border-radius` 只在挂到 `<svg>` 自己身上时才裁内容。** svg 根元素有 UA 默认的
`overflow:hidden`，所以圆角真的会裁掉画布；**挪到外层 `div` 上就不裁了**（实测：
只有 `border-radius` 的 div 里方角照样露出来），那时得额外补 `overflow-hidden`。
所以 `BrandMark` 的那几个类不能往外层搬。

**清单的扩展名是 `.json` 不是 `.webmanifest`。** 线上前端由 Go 的 `http.FileServer`
从二进制里吐（`server/httpd/static.go`），而 `mime.TypeByExtension(".webmanifest")`
在 Go 里返回**空字符串**——那时 Content-Type 靠内容嗅探，浏览器多半当 text/plain
处理并静默忽略整个清单。

## 深色模式：描边是算出来的，不是看出来的

logo 自带的深紫底 `#3C043C` 与深色页面底的对比度只有 **1.04:1**，边界等于不存在，
不处理就会糊成页面上的一块暗斑；浅色页面底那边是 15.8:1，根本不需要描边。
所以 `BrandMark` 挂了 `ring-1 ring-border`——`--border` 在两套主题里各有值，
于是自动是「浅色几乎看不见、深色刚好勾出边界」，不需要 `dark:` 变体
（本仓库全站不用它）。

用 `ring` 而不是 `border`：后者参与盒模型，会把 `size-9` 撑成 38px，
而品牌位在 flex 行里跟文字对齐，**那 1px 会让整行抖一下**。

## 怎么验证追踪没坏

脚本自己会验：把拟合出的贝塞尔路径**栅格化回原分辨率**，与原图掩码算 IoU，
低于 0.99 直接失败退出、不写产物。当前三个形状是 0.9945 / 0.9945 / 0.9935。

所以「换了源图但配色没跟着改 PALETTE」这类错会当场暴露，而不是产出一个
看着差不多的 logo。e2e 那侧的锁在 `web/e2e/01-workspace.spec.ts`
（图标文件真的取得到、不是命中 SPA fallback）与 `12-landing.spec.ts`（图元数是 4）。
