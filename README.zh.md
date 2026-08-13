[English](README.md) | 中文

![interest-memory](assets/banner.png)

# interest-memory — Agent 的长期记忆库

**一个 50MB 的进程，替代 Postgres + Redis + 向量库的一整套记忆后端。**

你的 agent 记不住事？每次会话都要重新介绍自己？这不是模型的错——是缺一个真正的记忆层。interest-memory 是一个独立的记忆服务：会话结束后从对话里提取兴趣点，核查、清洗、写入本地知识库；下次会话开始自动召回注入上下文。全部家当：**一个 18MB 二进制 + 一个 SQLite 文件**。记忆的单位是**兴趣点**：语义相近的自动合并，每个兴趣点由模型驱动的 agent loop 写成 wiki 页——知识库越用越收敛、不膨胀。

| 卖点 | 说明 |
|---|---|
| **轻** | 单个 ~18MB 二进制 + 一个 SQLite 文件就是全部；空载 ~17MB、峰值 <75MB（实测），树莓派都能跑 |
| **简单** | 一个二进制 + 一个配置文件即完整服务；`curl` 一键装，无外部数据库、无云依赖（LLM/embedding 可指向本地 Ollama/vLLM，完全离线） |
| **会话末提取** | 自动提取兴趣点 → 核查 → 写入本地知识库 |
| **会话初召回** | 自动召回相关记忆 → 注入上下文（只给精简条目，完整内容按需查，最小化上下文污染） |
| **多 agent 共享** | 一个服务接多个 agent（Hermes / OpenCode / Claude Code / Codex 等），记忆可隔离、全共享、或按需互通 |
| **全审计** | 每次结构化改动写入 `change_log`，可追溯回放 |
| **兴趣点收敛** | 语义相近的兴趣点自动合并/关联，而不是无脑堆条——记忆越用越收敛，不膨胀 |
| **归档演进** | 过期记忆标记 superseded/archived（不删除），带 replacement 链指向继任者；`GetByID` 可确认"什么取代了它"——旧知识不丢也不误导 |
| **语义边** | LLM 写入 wiki 时判定 5 类边：`related` / `contradicts` / `sequel` / `references` / `has_page`；结构变更 3 跳内级联传播（归档级联、后继替换、矛盾闭环、内容同步） |
| **由点到面** | 检索命中是记忆图的入口而非孤立片段：每条结果带出边/入边（id/title/kind/weight），`search?id=` 跳到节点再展开——从单点沿图走到邻域、再到整张网络，不再是单次 RAG 取片段 |
| **带证据** | 每条记忆都带证据（网页 / 会话轮次 / 检索 query）；主观偏好不当作事实；矛盾闭环处理 |

## 快速开始

**一键安装（curl）**

```bash
curl -fsSL https://raw.githubusercontent.com/djasdh/interest-memory/main/scripts/install.sh | bash
```

自动拉取源码 → 检查/安装依赖 → 引导配置 → 可选 systemd 自启。

**配置 LLM（让 agent 自己拉取安装）**

```bash
curl -fsSL https://raw.githubusercontent.com/djasdh/interest-memory/main/scripts/install_llm.py | python3 - --provider <provider>
# --help 查看全部 provider；交给 agent：它读 --help（即操作指令）自动完成配置
```

**预编译二进制（可选）**：[Release v0.1.0](https://github.com/djasdh/interest-memory/releases)（linux / mac / windows）

## 资源占用（实测）

在单测工作流上实测（Go 1.26 + cgo，`scripts/e2e.sh` 全链路）：

| 指标 | 数值 |
|---|---|
| 二进制大小 | ~18 MB（cgo 静态链接 sqlite-vec） |
| 空载内存 | **~17 MB RSS**（实测） |
| 流水线峰值内存 | <75 MB RSS |
| 初始占用 | ~20 MB（二进制 + 空库） |
| 持续使用增长 | 实测约 1 周涨至 ~38 MB，主体为会话原文转录（~71%）；其余为向量索引 + 兴趣点/wiki 页 |

`session_transcripts` 保留全文原文，想控磁盘增长可在外部定期清理；`fork.max_concurrency` / `verify.max_concurrency` 可压低峰值内存。

## 接入

现已接入多个 agent 框架，共用一套 env（`INTEREST_BASE_URL` / `INTEREST_AGENT` / `INTEREST_TIMEOUT`），服务挂了不阻塞会话：

| Agent | 接入形态 |
|---|---|
| Hermes | MemoryProvider 插件（`$HERMES_HOME/plugins/interest/`） |
| opencode | 本地插件（`~/.config/opencode/plugin/memory.ts`） |
| openclaw | 原生插件（`<configDir>/extensions/interest-memory/`） |
| pi | TS 扩展（`~/.pi/agent/extensions/interest-memory/`） |
| Claude Code | 官方插件 + MCP（`claude --plugin-dir bridge/claudecode`） |
| Codex | 官方插件 / hooks + MCP（`~/.codex/hooks.json`） |
| Reasonix | 官方插件 + MCP（`reasonix plugin install bridge/reasonix --link`） |
| DeepSeek Harness | Cordis 插件（`dsh plugin --profile web add @djasdh/interest-memory-dsh-bridge`，源码 `bridge/dsh/`） |

所有桥接能力一致：会话开始召回注入、会话结束推转录、消费端 `memory_search` / `memory_logs` 工具。详见 `bridge/README.md`。

## 架构

```
internal/store/      SQLite（兴趣点/wiki 页/边/claims/转录/change_log）
internal/vec/        sqlite-vec 向量索引（FTS 兜底）
internal/llm/        OpenAI 兼容 Chat/Embedding
internal/fork/       前缀窗口切分 + 并行候选提取
internal/verify/     三段式纠错（核查/claims/矛盾）
internal/wiki/       写入 agent loop + 相关页协同
internal/recall/     召回注入 + 结构化查询
bridge/hermes/       Hermes MemoryProvider 插件
```

## 文档

- **REST API** — `POST /api/v1/{agent}/sessions`、`GET /api/v1/{agent}/recall`、`search` / `logs` / `stats` / `jobs` 等（见下方 API 表）
- **配置** — `config.example.yaml` 全字段注释（llm / embedding / fork / verify / wiki / recall / namespaces）
- **开发** — `CGO_ENABLED=1 go test -race ./...`；插件测试 `node --test bridge/...`；端到端 `bash scripts/e2e.sh`

### API 速查

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/{agent}/sessions` | 会话末推转录 → 202 job_id |
| GET | `/api/v1/{agent}/recall?query=&after=&before=&days=` | 召回注入（时间过滤可选） |
| GET | `/api/v1/{agent}/search?query= 或 ?id=&top_k=` | 消费侧查询：完整内容 + 出边/入边；`?id=` 跳到节点，支持沿图遍历 |
| GET | `/api/v1/{agent}/logs?limit=&offset=` | 变更日志（倒序分页） |
| GET | `/api/v1/{agent}/interest-points` | 兴趣点列表 |
| GET | `/api/v1/{agent}/wiki/pages[?type=]` | wiki 页列表 |
| POST | `/api/v1/{agent}/fork` | 手动触发分叉 |
| GET | `/api/v1/{agent}/jobs/{id}` | 任务状态 |
| GET | `/api/v1/{agent}/stats` | 统计 |
| GET | `/api/health` | 健康检查 |

### 命名空间

每个 agent（`{agent}` 路径段 / `INTEREST_AGENT`）拥有独立命名空间，通过 `namespaces` 配置互通：

```yaml
namespaces:
  mode: isolated   # isolated（默认）| all（全部互通）| custom（指定互通）
  visible_to:      # 仅 custom：单向可见声明
    codex: [opencode, pi]
```

互通时结果标注来源（`recall` 行尾 `[from: <agent>]`，`search` 的 `result.agent` 字段）。

## 依赖

`my-agent-core`、mattn/go-sqlite3（cgo 静态链接）、sqlite-vec、goldmark-obsidian（双链解析）。全部 MIT 兼容。

## License

[MIT](LICENSE) — 贡献不分人写还是 AI 写，质量好就欢迎。
