# docs/guides —— 分场景的开发指引

这里是从 `CLAUDE.md` 拆出来的**按场景组织**的上下文。`CLAUDE.md` 只留每次都要知道的
东西（项目意图、仓库结构、常用命令、通用约定）加一张索引表；具体到某类改动才需要的
细节放这里，按需读。

| 文档 | 什么时候读 |
| --- | --- |
| [backend.md](backend.md) | 改 `server/` 下的 Go 代码 |
| [frontend.md](frontend.md) | 改 `web/src` 下的前端代码 |
| [heroui.md](heroui.md) | 用到 HeroUI v3 组件 |
| [styling.md](styling.md) | 改主题、配色、深色模式、圆角 |
| [i18n.md](i18n.md) | 增删界面文案 |
| [infra.md](infra.md) | 碰 `server/shared/infra/` 的外部接入层 |
| [docker-executor.md](docker-executor.md) | 部署 / 配置 Docker 执行端（会话容器），以及 docker.sock 的安全问题 |
| [host-executor.md](host-executor.md) | 碰「自带机器」执行端（`ama daemon`、`service/host`） |
| [environment.md](environment.md) | 装依赖、跑构建、找凭据、排查「本机跑不起来」 |

写这些文档的规矩和仓库其它地方一样：**踩过的坑要落到文档里**，不要只留在对话里。
坑属于哪个场景就写进哪个文件，通用性强的才提到 `CLAUDE.md`。
