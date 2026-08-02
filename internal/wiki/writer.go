package wiki

import (
	"context"
	"fmt"
	"strings"
	"time"

	"interest-memory/internal/llm"
	"interest-memory/internal/store"

	"my-agent-core/agent"
	"my-agent-core/provider"
	"my-agent-core/types"
)

// Compiler is the domain interface (design §七): write wiki pages from
// interest points + transcript via the agent loop, then rebuild edges.
// Compile returns the ids of pages touched this run (new/updated) so the
// caller can reconcile related pages.
type Compiler interface {
	Compile(ctx context.Context, agentID string, pts []store.InterestPoint, msgs []types.Message) ([]string, error)
	RebuildEdges(ctx context.Context, agentID string) error
	// ReconcileRelated propagates structural changes (written/updated pages,
	// archived interest points) to related pages within maxHops, in batches.
	ReconcileRelated(ctx context.Context, agentID string, in ReconcileInput, maxHops, batchSize int) error
}

// ProviderFactory builds the my-agent-core provider from config (used by the
// WikiWriter agent loop). Returns nil when the LLM is not configured.
type ProviderFactory func(ctx context.Context) (*provider.Provider, error)

// loopRunner executes one wiki agent loop. Injected for tests; production
// uses defaultRunLoop.
type loopRunner func(ctx context.Context, p *provider.Provider, system string, tools []types.Tool, prompt types.Message, emit types.EventSink) error

// Writer runs the SQLite-backed WikiWriter: for each interest point it spawns
// a dedicated my-agent-core agent armed with wiki_query / verify_claims /
// review / wiki_write tools to persist knowledge (single-point + evidence +
// conversation, serial loops).
type Writer struct {
	deps    ToolsDeps
	prov    ProviderFactory
	system  string
	timeout time.Duration
	runLoop loopRunner
}

// NewWriter builds a Writer. deps carries store/vec/embedder/llm/searcher;
// prov builds the LLM provider lazily per call so tests can inject a fake.
// lang is the output language hint injected into the system prompt
// (default "中文").
func NewWriter(deps ToolsDeps, prov ProviderFactory, lang string) *Writer {
	if lang == "" {
		lang = "中文"
	}
	system := `你是一个 wiki 编辑助手。你为单个兴趣点撰写或更新一个 wiki 页面。
流程（必须遵守）：
1. 用 wiki_query 检查是否已有相关页面；已有则用其 id 更新，而非新建
2. 撰写页面内容（markdown，可含 [[wikilink]] 双链指向相关页）
3. 对客观事实声明，用 verify_claims 联网核查后再写入
4. 正式写入前，必须调用 review 工具审查 draft，采纳合理建议
5. 用 wiki_write 写入：page_type=concept，传 interest_point_id（本兴趣点 id），
   提供恰当的 tags、edges、claims
6. 输出简洁准确，不要写无意义内容

语言：所有页面内容（标题、正文、相关链接）一律使用「` + lang + `」输出。标题中的英文专有名词（如项目名、工具名）可保留原文，但解释性文字必须使用该语言。

链接规则（必须遵守）：
- [[wikilink]] 的目标必须是 wiki_query 查到的、已存在页面的 id（如 [[pipeline-vs-agent-loop]]），
  禁止链接工具名（如 [[verify_claims]]）、外部概念（如 [[PostgreSQL]]）、抽象标签（如 [[design-decision]]）或本 wiki 中不存在的页面。
- 如果相关概念在本 wiki 中没有页面，不要用 [[]] 链接它，直接在正文中用普通文本提及。`
	return &Writer{
		deps: deps,
		prov: prov,
		system: system,
		timeout: 10 * time.Minute,
		runLoop: defaultRunLoop,
	}
}

func defaultRunLoop(ctx context.Context, p *provider.Provider, system string, tools []types.Tool, prompt types.Message, emit types.EventSink) error {
	ag := agent.NewAgent(p)
	ag.SystemPrompt = system
	ag.Tools = tools
	_, err := ag.Prompt(prompt, nil, emit, ctx.Done())
	return err
}

// toolsFor builds the per-point tool set.
func (w *Writer) toolsFor(agentID string) []types.Tool {
	return []types.Tool{
		NewQueryTool(w.deps, agentID),
		NewTagsTool(w.deps, agentID),
		NewVerifyClaimsTool(w.deps),
		NewReviewTool(w.deps, agentID),
		NewWriteTool(w.deps, agentID),
	}
}

// Compile writes one wiki page per interest point via a dedicated agent loop
// (serial, no parallelism — avoids write races). Each point's prompt carries
// its evidence, the exact conversation segment (by TurnRange), and a
// pre-looked-up related-page summary. Returns the page ids touched this run.
func (w *Writer) Compile(ctx context.Context, agentID string, pts []store.InterestPoint, msgs []types.Message) ([]string, error) {
	if w.prov == nil {
		return nil, nil
	}
	p, err := w.prov(ctx)
	if err != nil || p == nil {
		return nil, fmt.Errorf("wiki: provider: %w", err)
	}
	if len(pts) == 0 {
		return nil, nil
	}

	tools := w.toolsFor(agentID)
	var touched []string
	for _, ip := range pts {
		related := prelookupRelated(ctx, w.deps, agentID, ip)
		dialog := dialogSegment(msgs, ip.TurnRange)
		prompt := buildPointPrompt(ip, dialog, related)

		var pointTouched []string
		loopCtx, cancel := context.WithTimeout(ctx, w.timeout)
		err := w.runLoop(loopCtx, p, w.system, tools, types.Message{Role: types.RoleUser, Text: prompt}, func(ev types.Event) {
			if ev.Type == "tool_execution_end" && ev.ToolName == "wiki_write" {
				if id, ok := ev.Args["id"].(string); ok && id != "" {
					pointTouched = append(pointTouched, id)
				}
			}
		})
		cancel()
		if err != nil {
			return touched, fmt.Errorf("wiki: point %q: %w", ip.Name, err)
		}
		// Backfill EventTime on pages the agent wrote without event_time.
		if !ip.EventTime.IsZero() {
			w.backfillEventTime(ctx, agentID, pointTouched, ip.EventTime)
		}
		touched = append(touched, pointTouched...)
	}
	return touched, nil
}

// backfillEventTime stamps pages whose EventTime the agent omitted, using the
// interest point's event time (writer-level safety net).
func (w *Writer) backfillEventTime(ctx context.Context, agentID string, ids []string, et time.Time) {
	for _, id := range ids {
		p, err := w.deps.Store.GetPage(ctx, agentID, id)
		if err != nil || p == nil || !p.EventTime.IsZero() {
			continue
		}
		p.EventTime = et
		_ = w.deps.Store.UpsertPage(ctx, *p)
	}
}

// buildPointPrompt renders one interest point's write prompt: the point
// (name/summary/keywords/reliability/subjective/evidence), the exact dialog
// segment backing it, and the pre-looked-up related page summary.
func buildPointPrompt(ip store.InterestPoint, dialog, related string) string {
	var b strings.Builder
	b.WriteString("请为下面的兴趣点撰写或更新一个 wiki 页面。\n\n")
	b.WriteString("## 兴趣点\n")
	b.WriteString(fmt.Sprintf("- 主题: %s\n", ip.Name))
	if ip.Summary != "" {
		b.WriteString(fmt.Sprintf("- 摘要: %s\n", truncate(ip.Summary, 500)))
	}
	if len(ip.Keywords) > 0 {
		b.WriteString(fmt.Sprintf("- 标签: %s\n", strings.Join(ip.Keywords, ", ")))
	}
	if ip.Subjective {
		b.WriteString("- 主观性: 是（用户个人偏好/观点）——无需联网核查事实\n")
	} else {
		b.WriteString("- 主观性: 否（客观事实声明）——写入前用 verify_claims 联网核查\n")
	}
	if !ip.EventTime.IsZero() {
		b.WriteString(fmt.Sprintf("- 事件时间: %s（wiki_write 的 event_time 参数填这个）\n", ip.EventTime.UTC().Format("2006-01-02T15:04:05Z07:00")))
	}
	if ip.Reliability.Status != "" {
		b.WriteString(fmt.Sprintf("- 可信度: %s (%.2f)\n", ip.Reliability.Status, ip.Reliability.Confidence))
	}
	if len(ip.Reliability.Evidence) > 0 {
		b.WriteString("\n## 证据（写 claims 时引用来源）\n")
		for _, e := range ip.Reliability.Evidence {
			loc := e.SourceID
			if e.URL != "" {
				loc = e.URL
			}
			b.WriteString(fmt.Sprintf("- [%s] %s\n", e.Kind, truncate(loc, 200)))
			if e.Excerpt != "" {
				b.WriteString(fmt.Sprintf("  引用: %s\n", truncate(e.Excerpt, 200)))
			}
		}
	}
	if dialog != "" {
		b.WriteString("\n## 对应对话片段（来源轮次）\n")
		b.WriteString(dialog)
	}
	if related != "" {
		b.WriteString("\n## 已存在的相关页面（优先更新而非新建）\n")
		b.WriteString(related)
	}
	b.WriteString("\n请按规则：wiki_query → draft → verify_claims（客观声明）→ review → wiki_write（带 interest_point_id）。\n")
	return b.String()
}

// dialogSegment extracts the exact conversation segment backing an interest
// point, by its global message-index TurnRange (as mapped by fork).
func dialogSegment(msgs []types.Message, tr [2]int) string {
	if tr == [2]int{0, 0} {
		return ""
	}
	var llmMsgs []llm.Message
	for _, m := range msgs {
		llmMsgs = append(llmMsgs, toLLM(m))
	}
	s, e := tr[0], tr[1]
	if s < 0 {
		s = 0
	}
	if e >= len(llmMsgs) {
		e = len(llmMsgs) - 1
	}
	if s > e || s >= len(llmMsgs) {
		return ""
	}
	var b strings.Builder
	for _, m := range llmMsgs[s : e+1] {
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		switch m.Role {
		case "user":
			b.WriteString("[USER]: " + truncate(text, 400) + "\n")
		case "assistant":
			b.WriteString("[ASSISTANT]: " + truncate(text, 300) + "\n")
		}
	}
	return b.String()
}

func toLLM(m types.Message) llm.Message {
	role := "user"
	switch m.Role {
	case types.RoleAssistant:
		role = "assistant"
	case types.RoleToolResult:
		role = "tool"
	}
	return llm.Message{Role: role, Content: m.Text}
}

// prelookupRelated semantically searches the most relevant existing wiki page
// for the interest point and renders a compact summary the agent must use to
// update rather than duplicate.
func prelookupRelated(ctx context.Context, deps ToolsDeps, agentID string, ip store.InterestPoint) string {
	q := ip.Name
	if ip.Summary != "" {
		q += " " + ip.Summary
	}
	hits, err := semanticSearch(ctx, deps, agentID, q, 3)
	if err != nil || len(hits) == 0 {
		kw, kerr := keywordSearch(ctx, deps, agentID, q, 3)
		if kerr == nil && len(kw) > 0 {
			hits = kw
		}
	}
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	for _, h := range hits {
		b.WriteString(fmt.Sprintf("=== %s (score %.2f) [%s] ===\n", titleOf(h), h.Score, h.Kind))
		b.WriteString(fmt.Sprintf("ID: %s\n", h.ID))
		if body, ok := h.Meta["body"]; ok && body != "" {
			b.WriteString(fmt.Sprintf("Preview: %s\n", truncate(body, 300)))
		}
	}
	return b.String()
}

// RebuildEdges regenerates the adjacency table from page body wikilinks and
// persists contradictions bidirectionally (design §五 step 7).
func (w *Writer) RebuildEdges(ctx context.Context, agentID string) error {
	pages, err := w.deps.Store.ListPages(ctx, agentID, "")
	if err != nil {
		return fmt.Errorf("wiki: rebuild: list pages: %w", err)
	}
	for _, p := range pages {
		full, err := w.deps.Store.GetPage(ctx, agentID, p.ID)
		if err != nil {
			continue
		}
		if full == nil {
			continue
		}
		// Clear stale out-edges, then re-derive from wikilinks.
		if err := w.deps.Store.DeleteEdgesFor(ctx, agentID, p.ID); err != nil {
			return fmt.Errorf("wiki: rebuild: delete edges: %w", err)
		}
		// Refresh the pending (dead-link) set for this page: clear prior
		// records so removed links don't linger, then re-record current ones.
		if err := w.deps.Store.DeletePendingLinksFor(ctx, agentID, p.ID); err != nil {
			return fmt.Errorf("wiki: rebuild: delete pending links: %w", err)
		}
		links := ExtractWikilinks(full.BodyMD)
		for _, target := range links {
			if target == p.ID {
				continue
			}
			existing, err := w.deps.Store.GetPage(ctx, agentID, target)
			if err != nil || existing == nil {
				// Dead link: record it so the feedback loop can surface
				// (pending links) instead of silently dropping it.
				_ = w.deps.Store.RecordPendingLink(ctx, agentID, p.ID, target)
				continue
			}
			// Resolved: the target now exists — clear any prior pending record.
			_ = w.deps.Store.ClearPendingLink(ctx, agentID, p.ID, target)
			_ = w.deps.Store.AddEdgePair(ctx, agentID, store.Edge{
				SourceID:  p.ID,
				TargetID:  target,
				Kind:      store.EdgeReference,
				Weight:    1,
				CreatedAt: time.Now(),
			})
		}
	}
	return nil
}

// Helper: exported for tests.
func timeNow() time.Time { return time.Now() }
