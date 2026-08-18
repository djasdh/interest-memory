# 8.18 排期（T → V 全串行，可执行版）

> **执行方式**：本文件是排期+执行契约；**T 组用现有计划** `docs/superpowers/plans/2026-08-18-token-cache-optimization.md`；**V 组每个子阶段实施前**，先用 superpowers:writing-plans 产出 task 级 TDD 计划（含失败测试→实现→验证→commit），再按 subagent-driven-development 或 executing-plans 执行。全部基于 main 分支。
> **F 组已融合进 V 组**：F1(0.75) 内化为 V1.4 config 固化；F2/F3(full+full2 路线 + prompt 具体事实优化) 内化为 V-E；F4(judge 采样评测) 内化为 V4。
> **执行者须知**：各阶段「关键接口契约」来自当前 main 分支源码调研（行号以调研时为准，实现时须复核）。下游阶段引用的签名必须与上游实际实现一致；不一致时以代码为准并回改本文件。
> **进度**：✅ T 组、V-E、V1.1-V1.4 已完成并发布（v0.2.0/v0.2.1）；⬜ V2、V3、V4 待实施。

## 依赖链

```
✅T1→T2→T3→T4→T5 → ✅V-E → ✅V1.1 → ✅V1.2 → ✅V1.3 → ✅V1.4 → ⬜V2 → ⬜V3 → ⬜V4
                  （V 组依赖 T2：embedding 内容缓存，T 组先行交付）
```

## 全局验证命令（每阶段退出前必须全绿）

```bash
gofmt -l .
go vet ./...
CGO_ENABLED=1 go test -race ./...
```

## 全局提交规范

- 每个 Task / 每阶段一个 commit；信息风格对齐 `git log --oneline` 现状（`feat|fix|refactor|perf|test|docs(<scope>): <message>`）。
- 禁止在未验证（验证命令未通过）时 commit。

---

# T 组：token-cache A 章（序 1-5）✅ 已完成

**执行**：直接按 `docs/superpowers/plans/2026-08-18-token-cache-optimization.md` 的 Task 1-5 逐项执行。该计划已是 task 级 TDD（含测试代码、实现代码、commit 命令），无需重写。

**顺序**：T1 → T2 → T3 → T4 → T5（T2 是 V 组依赖，须交付）。
**退出标准**：5 个 Task 全部 commit；全局验证命令全绿。
**注意**：T3（recall 短 TTL 缓存）在 `private/dev` 分支有现成实现（`166420e`），但本排期基于 main，**在 main 上重新实现，不跨分支合入**。B 章（级联缓存 L0→L3 / 历史簇匹配 / 会话预热 / 孤立点批量化）仅记录，不实施。
**实际 commit**：`561417d`（LRU）、`896586c`（embedding 内容缓存）、`673462d`（recall 短 TTL）、`b43b826`（实体读去重）、`91a89ae`（usage 批量 flush）。

---

# V-E 提取路线化（序 6）✅ 已完成

**目标**：fork 支持 `route` 配置（`prefix` 'no_perfix'/ `full+full2`）

**前置**：T2 交付（embedding 缓存，V1 用；本阶段不强依赖，但需保持 T 组在库）。

**关键接口契约**
- `internal/config/config.go` `ForkConfig` 新增字段：
  ```go
  Route string `yaml:"route"` // "prefix"（默认，现状）| "full"（全窗口 + full2 追加）
  ```
  默认值 `Route: "prefix"`（`Default()` 内）。
- `internal/fork/fork.go`：
  - `Analyzer` 新增字段 `route string`；`NewAnalyzer` 从 `cfg.Route` 读入，空串视为 `"prefix"`。
  - 新增方法 `extractAdditional(ctx context.Context, turns []llm.Message, first []Candidate) ([]Candidate, error)`：把 `first` 的 Topic 列表喂给 LLM，prompt 明确「不要重复已有主题」，返回新增候选。
  - `Analyze` 按 route 分派：
    - `prefix`：现有 `SplitPrefixWindows` + 并行 `extract`（现状不变）。
    - `full`：单窗口（整会话渲染，可直接复用 `SplitPrefixWindows` 在 `len(pos)<userStep` 时产出的单窗口路径，或显式构造 `[][]llm.Message{turns}`）；先 `extract` 一次，再 `extractAdditional` 一次，两轮结果经 `dedupe` 合并。
- `internal/service/service.go:105`：`fork.NewAnalyzer(llmClient, cfg.Fork, cfg.Wiki.Selective)` 无需改签名（route 从 cfg 读入）。
- prompt 具体事实优化：`fork.go:207-230` 内联 prompt 追加「事实类别引导」——包括但不限于：具体数值/日期/版本、API 端点/路径、工具与命令用法、颜色/视觉细节、报错根因。参照 `docs/fork-window-strategy.md` §8.4（`89527b6b` 案例：记忆只有元信息、缺具体事实）。

**涉及文件**：`internal/config/config.go`、`internal/config/config_test.go`、`internal/fork/fork.go`、`internal/fork/fork_test.go`、`config.example.yaml`（fork 段落加 `route` 注释说明）。

**验证**：fork 单测新增用例（route 分派、full 两次调用、full2 追加不重复、prompt 含事实类别引导）；`go test ./internal/fork/ ./internal/config/`。可选：复用 `/tmp/forkcompare` 工具对 `.demo/memory.db` 会话做 full+full2 实测。
**commit**：`feat(fork): route full+full2 with additional extraction and fact-oriented prompt`
**完成定义**：route 可切换、full 路线产出 = full 候选 + full2 追加（字符串零重复），prompt 含事实类别引导，全局验证命令绿。
**实际 commit**：`1fa9c41`（route full+full2 + fact prompt）、`5263c37`（non-prefix 切窗 + full2 默认）、`5b61d06`（includeTool 开关）、`e02a586`（route 分派）。

---

# V1 统一裁决（序 7-10，拆 4 子阶段）✅ 已完成

**总目标**：重写 `internal/interest` 包替代「verify#1 → interest.Clean → verify#2」三段；实现完整的去重合并->聚蔟->裁决->入库

## V1.1 去重合并+候选对检索 + 聚类（序 7）✅ 已完成

**目标**：s1 去重合并：复用现有去重环节 + 求 EBD 向量（进缓存）>0.6 成簇（本次提取）（阈值参数化），**每簇一次 LLM** 判 merge/compose/keep 合并或分离兴趣点（实际实现含 LLM，非纯聚类）。s2 去重合并后检索兴趣点 EBD 向量（历史+本次提取）（已缓存）→ 连通分量；无法在 s2 连通的单独交由裁决；冲突 = **多个分量共享同一历史点**（如 a 连 a1/b1、b 连 b1/c1，b1 被两分量共享）→ 入裁决排队，按「共享历史点与各分量蔟主的最高相似度」定序（谁更亲近谁先裁决）。
**前置**：T2（embedding 缓存）。
**预留**: v1.2 裁决生成 ebd 缓存回调接口。（实际实现：FinalPoint 携带 Vec，V1.2/V1.3 直接复用，无需独立接口）

- `Embedder` 接口对齐 `internal/llm/embedder.go`（`Embed`），直接复用 `*llm.Embedder` 即可（T2 已加缓存）。
- **检索范围含历史库**：相似对既算本次候选内部（>0.75），也算「本次候选 vs 历史兴趣点 >0.8」；历史向量**取本体**（`vec.VectorIndex.Get` 新增接口，L2 归一化解码），不用 `Search` 的排名分。
- **冲突语义（实际实现）**：冲突 = 一个历史点被 ≥2 个 Component 共享。共享历史点被拆出 Components，只进 Conflicts 队列。

**验证**：fake embedder/vec 单测（连通分量正确性、阈值过滤、0.6/0.75/0.8 三档）；`go test ./internal/interest/`。
**commit**：`feat(interest): s1 dedupe-merge ...` / `feat(interest): s2 EBD pair clustering ...`
**完成定义**：连通分量正确、阈值参数化（0.6 清理簇 / 0.75 本次提取 / 0.8 历史兴趣点）、含历史库检索路径通、冲突队列排序正确。
**实际 commit**：`d75a54f`（vec.Get）、`85ff73c`（s1 DedupeMerge）、`2b588a4`（s2 Cluster）、`44b8e7c`（s2 冲突语义修正：共享历史点 + 亲和度排序）、`c533186`（s1 embed/簇 LLM 并行化）。

## V1.2 裁决 LLM（序 8）（并行）✅ 已完成

**目标**：每连通分量一次 LLM，输出合并/矛盾/元数据；孤立兴趣点批量出元数据。**本阶段不落库**（输出纯裁决结构，V1.3 消费）。生成 ebd。
**前置**：V1.1。

- 动作集（实际实现）：**`merge`（覆盖式合并，新信息覆盖历史点）/ `keep`（相关但不可 merge，新建；可带 `updates[]` 带动同蔟历史点 update）/ `archive`（推翻历史点，归档 + 新建）**。`updates[]` 是 keep 的伴随效果（如 Go 1.18→1.19）。
- **遗漏作废**：decisions 未覆盖分量内任一本次点 → 整分量作废（历史点不动、成员新建兜底、矛盾丢弃）。
- 矛盾判定遵循 v2 §3.3：矛盾候选 = 与 ≥0.75 相似对集**重叠**的候选对；prompt 说明「什么算矛盾」，LLM 判真伪。矛盾独立于冲突队列。
- 输出真实增加、更新、合并、删除的兴趣点，无以上关系不输出，落库阶段无关兴趣点无需更新所以无需重复落库。
- 不做事实核查（无网页检索、无旧档案 relation 判定）。
- 每个最终点携带 ebd（Vec），V1.3 直接复用。
- **tags 兜底**：LLM 在 merged 里漏给 tags 时，回退到该成员原始候选 Tags（避免 tag 丢失）。

（prompt 构建为 `buildAdjudicatePrompt` / `buildIsolatedPrompt`，见 `internal/interest/adjudicate.go`）。
**验证**：mockLLM 单测（prompt 内容、merge/keep/archive 解析、updates 带动、孤立点批量、遗漏作废、矛盾）。
**commit**：`feat(interest): V1.2 unified adjudication ...`
**完成定义**：裁决输出结构稳定、LLM 解析正确、不正确重试 llm 阶段。
**实际 commit**：`282f0ae`（V1.2 四动作裁决）、`6697c16`（merge/keep/archive + updates[]，去 DecidedPairs）、`51e9cf8`（tags 兜底）、`31153ff`（llm 全局指数退避重试）。

## V1.3 裁决落库（序 9）（加锁并行）✅ 已完成

**目标**：按 V1.2 裁决合并决策落库，产出**合并后的最终兴趣点**；整合字符串去重（`normalizeTopic`）。**本阶段仍是独立模块，不接入 service。** 重要：当前批次全部裁决完成才落库（包括裁决排队）

**前置**：V1.2。

- 字符串去重：复用 `internal/interest/merge.go` 的 `normalizeTopic`（小写 + 空白折叠）。
- 落库规则（实际实现）：`create`/`update` → `UpsertInterestPoint` + `vec.Upsert` + `AppendLog`；`archive` → 置 archived + `vec.Delete` + archive 日志；矛盾 → `UpsertContradiction` + `contradicts` 双向边。
- **related 边（程序生成）**：落库后对所有存活点两两算 `FinalPoint.Vec` 的 cos ≥ 0.50 → 建 `related` 边（weight=cos），不跳过任何 pair（`DecidedPairs` 已取消）。
- **跨会话合并**：裁决的 EBD 检索已含历史库（V1.1），本次 vs 历史的合并在此完成；参照 merge 语义但不重复跑 EBD（v2 §3.3「落库合并不重复 EBD」）。

**涉及文件**：`internal/interest/persist.go`、`persist_test.go`。
**验证**：store 单测（fakeStore：合并/新建/日志/去重/related）。
**commit**：`feat(interest): V1.3 persist adjudication ...`
**完成定义**：裁决落库正确、合并语义对齐 V1.2 裁决、日志完整。
**实际 commit**：`cd8b882`（V1.3 Persist + 程序 related）。

## V1.4 service 接线切换（序 10）✅ 已完成

**目标**：`ProcessSession` 从旧管线（verify#1 → Clean → verify#2）切换为新管线（`fork → DedupeMerge(s1) → Cluster(s2) → Adjudicate(V1.2) → Persist(V1.3) → wiki`），砍掉 verify#1/#2。
**实际 commit**：`41b4d4e`（config 阈值 ClusterSim/HistSim + SimilarityMerge=0.75）、`0e447e3`（service 接线切换）、`416e78d`（删除旧 verify#1/#2 + 旧 Cleaner，-2516 行）。

---

# V2 wikiloop 编排（序 11）（并行）⬜ 待实施

**目标**：wiki 写入从 per-point loop 改为 **EBD 簇分组**（每簇一个 loop，孤立单独 loop）；`wiki_write` 的 `interest_point_id` 单值→**数组**（多点 id，多对一 has_page）；新增 **IP_query** 工具。设计依据 v2 §3.4/§3.6。

**前置**：✅ V1.4 已落地（裁决落库后进入 wikiloop；wikiloop 的 EBD 仅**分组**不再合并，v2 §3.4）。

**关键接口契约**
- `internal/wiki/writer.go`：`Compiler.Compile` 输入形态从「per-point」改为「簇分组」：
  - 簇内 = 值得写页的（裁决 wiki_worthy=true）+ 跟随 EBD 的相关点（不值得单独写但相关）。
  - 每簇一个 loop；孤立兴趣点单独 loop。
  - 决策复用裁决的 wiki_worthy：a 单点写页 / b 多点写一页（多对一 has_page）/ c 更新某点已有页 / d `wiki_query` 并入非本组已有页；相关点可参与写页或仅作参考。
- `internal/wiki/tools.go`：
  - `wiki_write` 参数 `interest_point_id` 单值 → `interest_point_ids` 数组（`types.ArgsMap` 解析适配，`tools.go:185`、`writeWiki` L315-322 has_page 建边改为循环建边）。
  - 新增 `NewIPQueryTool(deps ToolsDeps, agentID string) types.Tool`：只搜兴趣点 + has_page 关系（范围含历史库），返回 name/summary/reliability + 已有页 id/title。
  - **has_page 只信声明**：系统按模型声明的多点 id 自动建/更新边，不推断。
  - **漏写兜底**：漏写必有原因 → 写 log，后续补写（v2 §3.4）。
- `internal/wiki/reconcile.go`：`ReconcileInput` 需感知多点 has_page（V4 再深入，本阶段保证编译通过、单测适配）。

**涉及文件**：`internal/wiki/writer.go`、`internal/wiki/tools.go`、`internal/wiki/tools_test.go`、`internal/wiki/writer_test.go`、`internal/wiki/reconcile.go`（最小适配）。
**验证**：wiki 单测（多对一 has_page 建边、IP_query 返回、簇分组 loop 数正确）；`go test ./internal/wiki/ ./internal/service/`。
**commit**：`feat(wiki): cluster-grouped wikiloop, multi-point has_page, IP_query tool`
**完成定义**：簇分组写页可用、多对一 has_page 建边正确、IP_query 含历史库、漏写日志落地。

---

# V3 可信反向写回（序 12）（并行）⬜ 待实施

**目标**：闭合可信流——wikiloop 写页时做的 verify_claims/review 复核结果，经 `wiki_write` 落页后**反向写回** `has_page` 源兴趣点的 reliability/freshness，带 `AppendLog` 审计。v2 §3.5。

**前置**：V2（多点 id 声明已就绪）。

**关键接口契约**
- `internal/wiki/tools.go` `writeWiki` 落页成功后，按声明的多点 id 逐个：
  1. `store.GetInterestPoint(agentID, id)` 查兴趣点；不存在（已归档/删除）→ **跳过**（防复活已作废记忆）。
  2. 用复核参数更新 `Reliability` / `Freshness`（未提供的字段保留原值），追加 evidence。
  3. `AppendLog(action="reliability_update")`（审计模式对齐 V1.3 Persist 的矛盾/更新落库：`internal/interest/persist.go`）。
  4. `has_page` 边按多点 id 批量建立。
- `wiki_write` 新增可选参数（`types.ArgsMap`）：
  ```go
  reliability_status string   // supported | contested | unknown
  confidence         float64  // 0-1
  freshness_level    string   // fresh | aging | stale | unknown
  ttl_days           int
  evidence           []string
  ```
- 复核优先：wikiloop 给了明确复核结果 → 覆盖初判；未复核字段保留统一裁决初判。
- 只读初判不丢：复核仅修正 reliability/freshness，不触碰合并/矛盾结论。
- ~~孤儿接口 `verify.FeedbackWrite`（`internal/verify/grade.go:109-127`）~~：**已废弃删除**（V1 清理 `416e78d`）。反向写回直接由 `wiki_write` 落页时执行，无需独立载体。

**涉及文件**：`internal/wiki/tools.go`、`internal/wiki/tools_test.go`。
**验证**：wiki/tools 单测（写回触发、归档跳过、字段覆盖优先级、日志）；`go test ./internal/wiki/ ./internal/interest/ ./internal/service/`。
**commit**：`feat(wiki): reliability/freshness writeback to driving interest points`
**完成定义**：反向写回闭环、归档跳过、审计日志、可信流闭合（统一裁决初判 → wikiloop 复核 → 回写定稿）。

---

# V4 reconcile 收尾 + 评测（序 13）⬜ 待实施

**目标**：reconcile 兼容多对一 has_page 的 cascade 语义（v2 §8 风险 8/9 兜底）；产出 judge 3 次采样评测脚本并端到端实证（F4 内化）。

**前置**：V2/V3 全部落地。

**涉及文件 / 内容**
- `internal/wiki/reconcile.go`：`ReconcileRelated` 的 `resolveArchived` / `collectRelated` / cascade 对多对一 has_page 的适配；相关点参与写页后的身份一致性抽查（v2 §8 风险 8）。
- **评测基础设施**（F4 内化，参照 `docs/fork-window-strategy.md` §9 的 `/tmp/forkcompare` 临时工具模式）：
  - judge 单次评分不稳定（`docs/fork-window-strategy.md` §4 观察），正式结论需 **3 次采样取均值**。
  - 评测脚本复用 `internal/fork` / `internal/store` / `internal/vec` / `internal/interest` / `internal/llm`；素材从 `.demo/memory.db` 的 `session_transcripts` 导出。
  - 对比：新管线（V1 统一裁决）vs 旧管线（verify#1/#2 快照）在相同会话上的 coverage/precision/granularity/redundancy/overall。**注**：旧管线代码已删除（`416e78d`），对比需从 git 历史恢复旧管线快照。
  - 依赖 env：`DEEPSEEK_API_KEY`、`SILICONFLOW_API_KEY`（bge-m3 embedding）。

**验证**：reconcile 单测（多对一 cascade、漏写补写日志）；评测脚本跑通并产出实测报告（写入 `docs/`）。
**commit**：`fix(wiki): reconcile multi-point has_page cascade` + `test(eval): judge 3-sample evaluation harness`
**完成定义**：reconcile 多对一语义正确；评测脚本可复现；实测报告落盘并记录新/旧管线差异。

---

## 未排期（方向记录，依赖 T2 与 V 组落地）

- **B1** 级联缓存（L0→L3：文本→向量→相似度→分组→裁决结论）
- **B2** 历史簇匹配（wikiloop 跳写 + 增量更新旧页）
- **B3** 会话级记忆预热（独立预热机制，风险高，需行为偏移实验）
- **B4** 孤立点批量化（V1.2 已有批量雏形，评估批大小与降级路径）

## 关键决策记录

- **V 组基于 main**；T3 在 `private/dev` 有现成实现但跨分支合入冲突大，main 上重做。
- **并行阶段 → V1.4 单点切换**：V1.1-V1.3 新增模块与旧管线并行存在，V1.4 验证通过后砍 verify#1/#2。✅ 已执行（`0e447e3` 切换、`416e78d` 删除旧管线 -2516 行）。
- **V1.4 固化 `similarity_merge=0.75`**（fork-window-strategy §5 实测支撑，F1 内化）。✅ 已执行（`41b4d4e` 默认值 0.85→0.75）。
- **V-E 先行**：full+full2 候选形态先定型，V1.2 裁决 prompt 按新形态设计，避免返工。✅ 已执行。
- **F 组融合**：F1→V1.4、F2/F3→V-E、F4→V4，无独立 F 组排期。
- **V1.2 动作语义（实施中细化）**：merge（覆盖式合并）/ keep（相关但不可 merge，可带 updates[] 带动历史点更新）/ archive（推翻归档）；无独立 update 动作。冲突 = 共享历史点的分量组，按共享点与蔟主最高亲和度定序。
