[English](README.en.md) | 中文

![interest-memory](assets/banner.png)

# interest-memory — Agent 的长期记忆库

**一个 50MB 的进程，替代 Postgres + Redis + 向量库的一整套记忆后端。**

你的 agent 记不住事？每次会话都要重新介绍自己？这不是模型的错——是缺一个真正的记忆层。interest-memory 是一个独立的记忆服务：会话结束后从对话里提取兴趣点，核查、清洗、写入本地知识库；下次会话开始自动召回注入上下文。全部家当：**一个 18MB 二进制 + 一个 SQLite 文件**。

## 为什么是它

| | interest-memory | mem0 | Zep | Letta |
|---|---|---|---|---|
| 部署形态 | **单个二进制 + SQLite** | Python 服务 + 向量库 | Postgres 全家桶 | Postgres + 多进程 |
| 空载内存 | **~17 MB**（实测） | 数百 MB | 数百 MB ~ 1G+ | 数百 MB+ |
| 外部依赖 | **无**（LLM 走 API） | 需要向量库 | Redis + Postgres | Postgres + 每 agent 进程 |
| 运行环境 | 树莓派都行 | 一台正经服务器 | 一台正经服务器 | 一台正经服务器 |

别人的记忆系统要一台服务器，你的只要一个进程。

**它能做什么**

- 会话结束：自动提取兴趣点 → 核查 → 写入本地知识库
- 会话开始：自动召回相关记忆 → 注入上下文
- 每条记忆都带证据（网页 / 会话轮次 / 检索 query）；主观偏好不当作事实；矛盾闭环处理

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

## 亮点

- **轻**：空载 ~17 MB、峰值 <75 MB（实测）；全部数据落在本地一个 SQLite 文件
- **自托管**：一个二进制 + 一个配置文件；无云依赖，可完全本地化
- **可信**：三段式纠错（核查 → 关系裁决 → 证据定位），主观性豁免，矛盾闭环
- **可控**：`change_log` 全审计可回放；命名空间隔离/互通可配；渐进式披露防上下文污染
- **能接入**：Hermes 插件开箱即用；其他 agent 走 REST API（OpenAI 兼容范式）

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

## 接入 Hermes

```bash
cp -r bridge/hermes $HERMES_HOME/plugins/interest/
# env: INTEREST_BASE_URL=http://127.0.0.1:8899 INTEREST_AGENT=<profile>
```

插件能力：会话开始 `prefetch` 召回注入、会话结束推转录、消费端 `memory_search` / `memory_logs` 工具。

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
| GET | `/api/v1/{agent}/search?query= 或 ?id=&top_k=` | 消费侧查询：完整内容 + 边关系 |
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
