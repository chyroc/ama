# OpenViking Memory (`ov_memory.py`) 命令与 URI 规范参考

## 1. 环境变量（平台已自动注入容器）

- `OPENVIKING_URL`：默认 `https://api.vikingdb.cn-beijing.volces.com/openviking`
- `OPENVIKING_API_KEY`：OpenViking 数据库访问密钥（Bearer Token）
- `OPENVIKING_RESOURCE_ID`：OpenViking 数据库实例 ID（如 `ov-example`）
- `OPENVIKING_SESSION_ID`：当前 AMA 会话 ID（`sesn-...`）

## 2. `viking://` 虚拟文件系统层级与分类

| 根路径 / 分类 | 说明 |
| --- | --- |
| `viking://user/default/memories/preferences/` | 用户偏好、风格约定、输出习惯、业务口径约定 |
| `viking://user/default/memories/entities/` | 实体档案（项目、品牌、商品、系统架构、联系人等） |
| `viking://user/default/memories/events/` | 重要事件、历史决策、里程碑记录 |
| `viking://user/default/memories/identity.md` | 用户身份与画像概览 |
| `viking://user/default/memories/soul.md` | Agent 角色与协作原则 |
| `viking://agent/` | 跨会话共享的 Agent 经验、技能与记忆 |
| `viking://resources/` | 外部导入的知识库、文档、网页或代码仓库资源 |

## 3. 分层渐进式披露（L0 / L1 / L2）

- **L0（`.abstract.md` / `abstract`）**：单句摘要，用于快速判断相关性（`search` / `find` 默认返回）。
- **L1（`.overview.md` / `overview`）**：结构化章节概览（`ov_memory.py read <uri> --level overview`）。
- **L2（正文全文 / `full`）**：完整原始文本（`ov_memory.py read <uri>` 或 `--level full`）。

## 4. 全部子命令速查

```bash
SCRIPT="./skills/openviking-memory/scripts/ov_memory.py"

# 1) 深度语义检索（支持会话上下文分析与分类过滤）
python3 "$SCRIPT" search "品牌 ROI 核算口径" --limit 5
python3 "$SCRIPT" search "季度复盘" --mode context --max-tokens 1500
python3 "$SCRIPT" search "投放复盘" --context-type memory --target-uri viking://user

# 2) 快速语义查找（无会话重写的轻量级向量召回）
python3 "$SCRIPT" find "用户偏好" --limit 5

# 3) 读取指定 URI 的内容（支持 L0 / L1 / L2 与行区间分页）
python3 "$SCRIPT" read viking://user/default/memories/identity.md
python3 "$SCRIPT" read viking://user/default/memories/preferences --level overview
python3 "$SCRIPT" read viking://resources/doc.md --offset 0 --limit 200

# 4) 浏览目录与树结构
python3 "$SCRIPT" list viking://user/default/memories --recursive
python3 "$SCRIPT" tree viking:// --depth 3 --include-abstract

# 5) 关键词正则搜索与文件名匹配
python3 "$SCRIPT" grep "ROI" --uri viking://user --ignore-case
python3 "$SCRIPT" glob "**/*.md" --uri viking://user

# 6) 主动写入长期记忆（自动抽取并归档到长期记忆目录）
python3 "$SCRIPT" remember "用户要求：所有 ROI 报表默认同时展示平台直接归因 GMV 口径与订单库含复购口径。"

# 7) 直接创建/覆盖或局部编辑指定 viking:// 文件
python3 "$SCRIPT" write viking://user/default/memories/preferences/roi_standard.md "# ROI 口径规范\n..." --mode replace --wait
python3 "$SCRIPT" edit viking://user/default/memories/preferences/roi_standard.md --old "旧文本" --new "新文本" --wait

# 8) 删除记忆或目录（不可逆，执行前需确认）
python3 "$SCRIPT" forget viking://user/default/memories/preferences/obsolete.md

# 9) 导入外部 URL 或文档到知识库
python3 "$SCRIPT" add-resource "https://example.com/spec.md" --to "viking://resources/spec" --description "产品规范文档"

# 10) 检查连接与记忆库健康状态
python3 "$SCRIPT" status
```
