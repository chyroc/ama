# AMA 前端

Vite + React 19 + TypeScript + Tailwind CSS v4 + HeroUI v3。

设计文档在 `../docs/DESIGN.md`——信息架构、数据模型命名约定、33 类事件的呈现策略都在那里，
动手改之前先读。踩过的坑与约定按场景拆在 `../docs/guides/` 下：组件坑见 `heroui.md`
（Modal 嵌套、line-clamp）、视觉约定见 `styling.md`（主题、圆角分级）、文案见 `i18n.md`、
环境问题见 `environment.md`（inotify 等）。

## 跑起来

```bash
npm install
./scripts/dev.sh      # http://127.0.0.1:5273（PORT 可覆盖）

# 另开一个终端起后端，前端默认打它：
cd ../server && AMA_DEV_ROUTES=1 go run ./cmd/amad
```

只想调 UI、不想起后端：`VITE_API_MODE=local ./scripts/dev.sh`。

不要直接 `npx vite`：本机 inotify 实例被占满，需要脚本里的 ulimit 与轮询配置。

```bash
npm run typecheck
npm run build
npm run e2e           # 59 个用例，用系统 Chrome，无需 playwright install
```

## 目录

```
src/
  types/
    events.ts       方舟 33 类会话事件的判别联合 + 展示层分类
    domain.ts       workspace / agent / skill / feishu / run，含模型参数表
  data/
    api.ts          ApiClient 接口 + LocalApiClient（localStorage 实现）+ 选哪份实现
    http.ts         HttpApiClient（默认），对接 server/ 的 Go 服务
    seed.ts         种子数据，含一条覆盖多种事件类型的复杂运行
  lib/
    timeline.ts     事件流 → 时间线条目（工具调用配对、分类、统计汇总）
    format.ts       时间/时长/token 格式化
  app/
    WorkspaceGate.tsx   校验 URL 里的 workspace key，取不到就跳引导页
    AppShell.tsx        侧栏 + 内容区
  components/
    Page.tsx        PageHeader / PageBody / Empty / 骨架屏
    FormModal.tsx   表单弹窗与危险操作确认的公共骨架
    RunTimeline.tsx 事件时间线
  pages/            按模块一个文件
  router.tsx        路由表
e2e/                Playwright 用例，按需求编号分文件
```

## 两个关键设计

**workspace 优先**落在三处：路由（业务路由全在 `/w/:workspaceKey` 下）、网关
（`WorkspaceGate` 先校验再渲染）、空态（没有空间时强制引导创建，无跳过入口）。

**运行历史的降噪**：33 类事件平铺不可读，所以 `lib/timeline.ts` 把 `tool_use` 与
`tool_result` 按 id 配对成单条（条目数少三分之一，也更符合「调用了一次工具」的心智），
再按语义分成 8 类给视觉身份；UI 侧提供类别筛选（默认隐藏 `span.*`）、只看对话、
按线程切换，每条都能展开原始 payload 兜底。

## 数据层

页面只依赖 `ApiClient` 接口，两份实现：`HttpApiClient`（`data/http.ts`，**默认**，
打 `server/` 的 Go 服务）与 `LocalApiClient`（`data/api.ts`，localStorage + 种子数据，
`VITE_API_MODE=local` 时启用）。

接口前缀默认是同源 `/api`，由 vite 的 `server.proxy` 转给 `127.0.0.1:8080`，所以
**要先起后端**。两个环境变量（`VITE_API_BASE` / `VITE_API_MODE`）
声明在 `src/vite-env.d.ts`，细节与踩过的坑见 `../docs/guides/frontend.md` 的「数据层」。

`window.__amaApi` 暴露 `reset()` / `clear()` 供 e2e 准备数据；`completeFeishuAuth()`
只有 LocalApiClient 有，所以 e2e 跑在 `VITE_API_MODE=local` 下（端口 5274，不碰 5273）。
