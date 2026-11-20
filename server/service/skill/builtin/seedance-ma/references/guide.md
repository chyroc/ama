# Seedance MA 视频与组图创作参考指南

## 1. 架构说明

`seedance-ma` 将火山方舟（Volcengine Ark）线上托管的 **Seedance MA** 视频与视觉创作智能体封装为 AMA Agent 可直接调用的命令行工具：

- **分镜规划**：自动完成创意拆解、分镜时间轴设计、运镜与光影提示词扩写
- **视频生成**：支持 4~15 秒高清视频生成、首尾帧控制与参考图外观保持
- **组图/海报生成**：支持风格版式高度统一的单图与多张系列组图生成
- **多镜头剪辑合成**：支持多镜头分镜生成后拼接成片

连接参数由平台通过 `ARK_SEEDANCE_API_KEY`、`ARK_SEEDANCE_AGENT_ID`、`ARK_SEEDANCE_ENVIRONMENT_ID`、`ARK_SEEDANCE_API_BASE_URL` 环境变量注入（也可通过命令行参数覆盖）。

## 2. 命令与参数

### `generate` — 创建并启动视频创作任务

```bash
python3 ./skills/seedance-ma/scripts/seedance_ma.py generate \
  --prompt "极具高级感的产品微距广告镜头：丝滑水润光泽感，暖金晨光，慢动作4K" \
  --title "产品微距广告" \
  --ratio 16:9 \
  --duration 5 \
  --wait-seconds 85
```

常用参数：
- `--prompt`（必填）：视频创意描述、剧情、产品卖点或分镜要求
- `--title`（可选）：视频短标题
- `--ratio`（可选）：画幅比例，支持 `16:9`（默认横屏）、`9:16`（竖屏短视频）、`1:1`、`4:3`、`3:4`、`21:9`
- `--duration`（可选）：单段视频秒数，范围 `4`~`15`，默认 `5`
- `--reference-images`（可选）：参考图 URL（多个用英文逗号分隔），可用于首帧图生视频或商品外观保持
- `--multi-clip`（可选开关）：当用户明确要求多镜头拼接长视频时开启
- `--wait-seconds`（可选）：单次工具调用最长轮询秒数，默认 `85`（上限 `95`）

### `watch` — 续接轮询正在渲染中的视频任务

当 `generate` 或上一次 `watch` 返回 `data.outcome == "in_progress"` 时，直接按返回的 `next.command` 继续执行：

```bash
python3 ./skills/seedance-ma/scripts/seedance_ma.py watch \
  --session-id <sesn-...> \
  --wait-seconds 85
```

通常调用 1~3 次 `watch` 即可拿到 `data.outcome == "completed"` 及可播放的 `data.video_url`。

### `generate-images` / `watch-images` — 生成单图或系列组图

当用户要求使用 Seedance MA **生图**或制作**组图**（如 3~4 张风格版式统一的系列海报）时，调用 `generate-images`：

```bash
python3 ./skills/seedance-ma/scripts/seedance_ma.py generate-images \
  --title "产品核心特性 4 张组图" \
  --ratio 16:9 \
  --count 4 \
  --wait-seconds 85 \
  --prompt "制作一套 4 张统一黑金科技版式的组图海报，分别介绍第 1 张……第 4 张……"
```

若返回 `data.outcome == "in_progress"`，继续执行返回 JSON 中的 `next.command`（`watch-images --session-id <sesn-...> --count 4`），直到 `data.outcome == "completed"` 拿到 `data.images`（每张含本地路径 `local_path` 与可直接访问的 `image_url`）。

## 3. 输出字段说明

- `data.outcome`：
  - `completed`：已生成完毕，`data.video_url` 为可直接播放的 MP4 链接，`data.local_path` 为下载到本地的文件路径
  - `in_progress`：分镜规划或视频渲染仍在进行中，需继续调用 `next.command`
  - `failed`：生成失败，见 `error`
- `data.storyboard_prompt`：自动设计的专业分镜与运镜提示词
- `data.video_url`：可在浏览器中直接内联播放与下载的 HTTPS MP4 视频地址
- `data.local_path`：保存在工作区内的本地 MP4 路径（默认在 `./outputs/videos/` 下）
