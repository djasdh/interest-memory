package wiki

import (
	"context"
	"fmt"
	"strings"
	"time"

	"interest-memory/internal/store"
	"interest-memory/internal/transcript"

	"my-agent-core/agent"
	"my-agent-core/provider"
	"my-agent-core/types"
)

// Compiler is the domain interface (design §七): write wiki pages from
// interest points + transcript via the agent loop, then rebuild edges.
type Compiler interface {
	Compile(ctx context.Context, agentID string, pts []store.InterestPoint, msgs []types.Message) error
	RebuildEdges(ctx context.Context, agentID string) error
}

// ProviderFactory builds the my-agent-core provider from config (used by the
// WikiWriter agent loop). Returns nil when the LLM is not configured.
type ProviderFactory func(ctx context.Context) (*provider.Provider, error)

// Writer runs the SQLite-backed WikiWriter: it spawns a my-agent-core agent
// armed with wiki_query / wiki_write tools to persist knowledge (design §五
// step 6 — all writes go through the agent loop).
type Writer struct {
	deps   ToolsDeps
	prov   ProviderFactory
	system string
	timeout time.Duration
}

// NewWriter builds a Writer. deps carries store/vec/embedder; prov builds
// the LLM provider lazily per call so tests can inject a fake.
func NewWriter(deps ToolsDeps, prov ProviderFactory) *Writer {
	return &Writer{
		deps: deps,
		prov: prov,
		system: `你是一个 wiki 编辑助手。你的任务是根据兴趣点和会话记录，将有用的知识写入 SQLite wiki。
规则：
- 使用 wiki_query 工具先检查是否已有相关页面；已有则更新而非新建
- 每个兴趣点写一个 concept 页面（title 用兴趣点主题，content 用 markdown，可含 [[wikilink]] 双链指向相关页）
- 会话支撑材料写 source 页面（page_type=source，session_ids 填来源会话）
- 多兴趣点的综合摘要写 synthesis 页面
- 添加合适的 tags
- 输出简洁准确，不要写无意义内容`,
		timeout: 10 * time.Minute,
	}
}

// Compile writes wiki pages from interest points + compressed transcript via
// the agent loop (design §五 steps 5-6).
func (w *Writer) Compile(ctx context.Context, agentID string, pts []store.InterestPoint, msgs []types.Message) error {
	if w.prov == nil {
		return nil
	}
	p, err := w.prov(ctx)
	if err != nil || p == nil {
		return fmt.Errorf("wiki: provider: %w", err)
	}
	if len(pts) == 0 {
		return nil
	}

	compressed := transcript.Compress(msgs)
	prompt := buildCompilePrompt(pts, compressed)
	ag := agent.NewAgent(p)
	ag.SystemPrompt = w.system
	ag.Tools = []types.Tool{
		NewQueryTool(w.deps, agentID),
		NewWriteTool(w.deps, agentID),
	}

	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	_, err = ag.Prompt(types.Message{Role: types.RoleUser, Text: prompt}, nil, func(types.Event) {}, ctx.Done())
	return err
}

func buildCompilePrompt(pts []store.InterestPoint, msgs []types.Message) string {
	var b strings.Builder
	b.WriteString("根据以下兴趣点和会话记录，为 wiki 撰写或更新文章。\n\n")

	b.WriteString("## 兴趣点\n\n")
	for i, ip := range pts {
		b.WriteString(fmt.Sprintf("%d. 主题: %s\n", i+1, ip.Name))
		if ip.Summary != "" {
			b.WriteString(fmt.Sprintf("   摘要: %s\n", truncate(ip.Summary, 500)))
		}
		if len(ip.Keywords) > 0 {
			b.WriteString(fmt.Sprintf("   标签: %s\n", strings.Join(ip.Keywords, ", ")))
		}
		if ip.Reliability.Status != "" {
			b.WriteString(fmt.Sprintf("   可信度: %s (%.2f)\n", ip.Reliability.Status, ip.Reliability.Confidence))
		}
		if len(ip.SourceSessions) > 0 {
			b.WriteString(fmt.Sprintf("   来源会话: %s\n", strings.Join(ip.SourceSessions, ", ")))
		}
	}

	b.WriteString("\n## 会话记录（已压缩）\n\n")
	for _, m := range msgs {
		switch m.Role {
		case types.RoleUser:
			b.WriteString(fmt.Sprintf("USER: %s\n", truncate(m.Text, 400)))
		case types.RoleAssistant:
			b.WriteString(fmt.Sprintf("ASSISTANT: %s\n", truncate(m.Text, 300)))
		case types.RoleToolResult:
			b.WriteString(fmt.Sprintf("TOOL(%s): %s\n", m.ToolName, truncate(m.Text, 150)))
		}
	}

	b.WriteString("\n请先用 wiki_query 检查相关文章，然后使用 wiki_write 创建或更新页面。\n")
	b.WriteString("对每个兴趣点，用 wiki_write 写入一个 concept 页；将会话摘要写入 source 页；需要时写 synthesis 页。\n")
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
		links := ExtractWikilinks(full.BodyMD)
		for _, target := range links {
			if target == p.ID {
				continue
			}
			existing, err := w.deps.Store.GetPage(ctx, agentID, target)
			if err != nil || existing == nil {
				continue
			}
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
