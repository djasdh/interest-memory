[English](README.en.md) | 中文

![interest-memory](assets/banner.png)

# interest-memory — 兴趣点记忆服务

独立 Go 记忆服务：会话末从对话中提取兴趣点，经核查/清洗后写入 wiki 双链知识库；会话开始 RAG 召回注入上下文。支撑 Hermes 等消费 agent 的长期记忆。

## 设计卖点

- **轻量** — 单二进制（~19 MB，cgo 静态链接 sqlite-vec），空载内存 ~20–40 MB、流水线峰值 <75 MB（实测，见下方基准）；全部数据落在本地一个 SQLite 文件，无外部数据库
- **自托管友好** — 一个二进制 + 一个配置文件即完整服务；无云依赖（LLM/embedding 走 OpenAI 兼容 `base_url`，可指向本地 Ollama/vLLM 等实现完全离线）；`scripts/install.sh` 引导安装 + systemd 自启
- **严格记忆审计** — 每次结构化改动写入 `change_log`（标题 + 动作 + 结构性边），可追溯回放；证据定位到网页 URL / 会话轮次 / 检索 query；claims 提取 + 矛盾检测闭环
- **主观性豁免** — 判断为「用户自身偏好/观点」的兴趣点跳过联网事实核查，但仍走 LLM 裁决，避免把主观倾向当事实写入
- **渐进式披露** — 会话注入只给精简条目（`recall`：top_k + 分数门槛 + 截断），完整内容 + 边关系按需查（`memory_search`），最小化上下文污染

## 核心特性

- **前缀窗口并行提取** — 每 5 个 user 回合一个前缀窗口步长（少于 5 不切分），并行分发提取，命中 DeepSeek/SiliconFlow 前缀缓存
- **三段式纠错** — 主观性豁免联网核查；新候选与最相似历史旧点关系裁决（supersede/update/delete → 归档/合并/新建）；证据定位（网页 URL / 会话轮次 / 检索 query）
- **每兴趣点独立 agent loop 写入** — 单点 + 证据 + 对应对话片段；工具集 `wiki_query / wiki_tags / verify_claims / review / wiki_write`（联网审计 + 只读 review + tag 分类法）
- **相关页协同** — 写入/归档变更后，≤3 跳图传播统一处理：级联归档（页标 superseded）、矛盾闭环、内容协同，超 10 页自动分批
- **消费端查询** — `recall` 精简注入 + `memory_search`（完整内容 + 边关系）/ `memory_logs` 工具
- **时间能力** — `session_date` 透传 → 事件时间 EventTime → recall 时间过滤（after/before/最近 N 天），支撑 LongMemEval temporal 评测
- **完整审计** — change_log 记录每次结构化改动（标题 + 动作 + 结构性边）；tag 分类法（`ListTags` 聚合 + `wiki_tags` 工具）

## 资源占用基准（实测）

在单测工作流上实测（Go 1.26 + cgo，`scripts/e2e.sh` 全链路）：

| 指标 | 数值 |
|---|---|
| 二进制大小 | ~19 MB（cgo 静态链接 sqlite-vec） |
| 空载内存（无 job 待命） | ~20–40 MB RSS |
| 流水线峰值内存 | <75 MB RSS（并行窗口 + 核查 + agent loop） |
| 空库磁盘 | ~88 KB（SQLite 单文件） |
| 初始占用 | ~20 MB（二进制 + 空库） |
| 持续使用增长 | 实测约 1 周涨至 ~38 MB，主体为会话原文转录（~71%）；其余为向量索引 + 兴趣点/wiki 页 |

宣传口径「50 MB 内存」对应**空载/典型**使用，峰值会到 ~75 MB；「20 MB 硬盘」为**初始**占用，后续随记忆与转录积累增长。可按需调低并行度（`fork.max_concurrency` / `verify.max_concurrency`）压低峰值内存；`session_transcripts` 保留全文原文，如需控制磁盘增长可在外部定期清理。

## 架构与数据流

```
会话末 POST /sessions（Hermes 推送转录，含 session_date）
  → worker 串行（每 agent）
  → fork      前缀窗口切分（每 5 user 回合）→ 并行侧 LLM 提取候选 + 去重
  → verify#1  并行核查：主观性豁免联网 / 关系裁决 / 证据定位
  → interest  按关系归档/合并/新增（EventTime/TurnRange 落库）
  → verify#2  claims 提取 + 矛盾检测
  → wiki      每兴趣点独立 agent loop 写页（verify_claims → review → wiki_write）
  → RebuildEdges  双链重建
  → reconcile 相关页协同（≤3 跳，级联/矛盾闭环/内容协同）

会话开始 GET /recall?query=
  → embed → vec 检索 → 时间过滤（after/before/days）→ 注入精简条目（含 (at 日期) 时间戳）

消费端工具：memory_search（query/id + 完整内容 + 边）/ memory_logs（变更日志）
```

## 快速开始

**前置**：Go 1.25+、CGO（sqlite-vec 静态链接）、LLM + embedding API key。

```bash
# 0. 一键安装（引导式向导：构建服务端 + 供应商配置 + 可选 agent bridge）
bash scripts/install.sh
#    install.sh 会按发行版检查/自动安装依赖（python/go/node/npm/curl），再进入 TUI
#    可选参数：--dry-run 只打印步骤 | --noninteractive 全默认 | --server-only 只配服务端 | --systemd 注册开机自启

# 1. 构建（单二进制）
go build ./cmd/server

# 2. 配置
cp config.example.yaml config.yaml
# 编辑 config.yaml：LLM（默认 DeepSeek）、embedding（默认 SiliconFlow BAAI/bge-m3）
# 从环境变量提供密钥
export LLM_API_KEY=...         # LLM 核查/提取/写入
export SILICONFLOW_API_KEY=...  # embedding（BAAI/bge-m3, 1024 维）

# 完全本地化：LLM/embedding 都走 OpenAI 兼容端点时，把 llm.base_url /
# embedding.base_url 指向本地 Ollama / vLLM / LM Studio 等，即可零云端依赖运行。
# 见 config.example.yaml 顶部示例。

# 3. 运行（默认 :8899）
./server -config config.yaml

# 4. 端到端测试（真实 LLM）
bash scripts/e2e.sh
```

## REST API

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/{agent}/sessions` | 会话末推转录（含可选 `session_date` RFC3339）→ 202 job_id |
| GET | `/api/v1/{agent}/recall?query=&after=&before=&days=` | 召回注入（时间过滤可选） |
| GET | `/api/v1/{agent}/search?query= 或 ?id=&top_k=` | 消费侧查询：完整内容 + 边关系 |
| GET | `/api/v1/{agent}/logs?limit=&offset=` | 变更日志（倒序分页） |
| GET | `/api/v1/{agent}/interest-points` | 兴趣点列表 |
| GET | `/api/v1/{agent}/wiki/pages[?type=]` | wiki 页列表 |
| POST | `/api/v1/{agent}/fork` | 手动触发分叉 |
| GET | `/api/v1/{agent}/jobs/{id}` | 任务状态 |
| GET | `/api/v1/{agent}/stats` | 统计 |
| GET | `/api/health` | 健康检查 |

## 命名空间与互通

每个 agent（`{agent}` 路径段 / `INTEREST_AGENT`）拥有独立命名空间：全部数据
表与向量索引按 `agent_id` 隔离，写入始终只进本空间。

读取侧（`recall` / `search` / 按 `id` 查询）的互通通过 `namespaces` 配置切换：

```yaml
namespaces:
  mode: isolated   # isolated（默认，不互通）| all（全部互通）| custom（指定互通）
  visible_to:      # 仅 custom：单向可见声明
    codex: [opencode, pi]
```

- `isolated`：每个 agent 只读自己的记忆（默认，无配置即现状）
- `all`：每个 agent 读取所有命名空间（服务端动态发现全集）
- `custom`：按 `visible_to` 单向声明（A 可读 B，B 不自动读 A）
- 互通时结果标注来源：`recall` 文本行尾 `[from: <agent>]`，`search`/`get` 的 `result.agent` 字段
- `logs` / `stats` 始终归属本空间，不参与互通

## Hermes 接入

部署插件到 Hermes 插件目录：

```bash
cp -r bridge/hermes $HERMES_HOME/plugins/interest/
```

配置 env：`INTEREST_BASE_URL`（默认 `http://127.0.0.1:8899`）、`INTEREST_AGENT`（agent 命名空间，默认 profile）、`INTEREST_TIMEOUT`。

插件能力：会话开始 `prefetch` 召回注入、会话结束推转录（含 `session_date`）、消费端 `memory_search` / `memory_logs` 工具。

## 配置参考

见 `config.example.yaml`。核心段：

- `server` — 监听地址 / 端口 / SQLite 路径
- `llm` — 侧 LLM（提取/核查/写入），base_url/api_key_env/model 独立可配
- `embedding` — 独立可配，默认 SiliconFlow `BAAI/bge-m3`（1024 维）
- `fork` — 前缀窗口步长(5) / 上限(8) / 并行度(4) / 相似度阈值
- `verify` — 联网核查开关 / search_max / web_tool / 并行度
- `wiki` — 协同传播深度(3) / 分批(10)
- `search` — 消费侧查询 top_k(3) / max_body_len(4000)
- `log` — 变更日志保留条数（0=无限）
- `recall` — top_k(8) / include_wiki / min_score(0.30)

## 目录结构

```
cmd/server/          入口（-config 标志，装配 + 优雅关闭）
internal/config/     YAML + env 覆盖配置
internal/store/      SQLite（兴趣点/wiki 页/边/claims/转录/change_log）
internal/vec/        sqlite-vec 向量索引（FTS 兜底）
internal/llm/        OpenAI 兼容 Chat/Embedding
internal/fork/       前缀窗口切分 + 并行候选提取
internal/verify/     三段式纠错（核查/claims/矛盾/召回标注）
internal/interest/   清洗（合并/关联/新增/归档）
internal/wiki/       写入 agent loop（5 工具）+ 相关页协同
internal/recall/     召回注入 + 结构化查询
internal/service/    编排层
internal/worker/     每 agent 串行队列
internal/httpapi/    REST 端点
internal/websearch/  可注册网络工具 registry
bridge/hermes/       Hermes MemoryProvider 插件
```

## 测试

```bash
# Go 全量（含 race）
CGO_ENABLED=1 go test -race ./...

# Hermes 插件
python3 bridge/hermes/test_interest.py

# 端到端（需 LLM_API_KEY + SILICONFLOW_API_KEY）
bash scripts/e2e.sh
```

## 依赖

`my-agent-core`（`github.com/djasdh/my-agent-core v0.1.0`）、mattn/go-sqlite3（cgo 静态链接）、sqlite-vec、goldmark-obsidian（双链解析）。
