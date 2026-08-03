package wiki

import (
	"context"
	"fmt"
	"strings"

	"interest-memory/internal/store"

	"github.com/djasdh/my-agent-core/types"
)

// ReconcileInput lists what changed this pipeline run so the reconcile stage
// knows which pages to propagate changes to.
type ReconcileInput struct {
	TouchedPages   []string // wiki page ids written/updated this run
	ArchivedPoints []string // interest point ids archived (deleted/superseded) this run
}

// reconcileSystem guides the unified reconcile agent loop: it propagates a
// structural change to related pages (级联归档 / 矛盾闭环 / 内容协同). It runs
// WITHOUT review — the changes are executed directly.
const reconcileSystem = `你是一个 wiki 协同编辑助手。某个页面或兴趣点刚刚发生了结构性变更，你需要统一修改与其相关的 wiki 页面，保持知识库自洽。
规则：
- 用 wiki_query 查询相关页，用 wiki_write 修改（可传 status 参数，如 superseded/archived）
- 级联归档：被取代/推翻的旧页应标记 status=superseded（必要时删除其引用），不再作为当前知识
- 矛盾闭环：若某相关页与变更后的事实直接矛盾，修正或标记其状态
- 内容协同：相关页中提到已变化内容的地方应更新为最新事实
- 不要新增与本次变更无关的页面
- 输出简洁，直接执行修改`

// ReconcileRelated propagates a structural change to related wiki pages
// within maxHops hops (default 3). It collects the related-page subgraph
// (outlinks/backlinks/has_page), then dispatches batches of up to batchSize
// pages to a unified agent loop (wiki_query + wiki_write only — no review)
// that performs 级联归档 / 矛盾闭环 / 内容协同.
func (w *Writer) ReconcileRelated(ctx context.Context, agentID string, in ReconcileInput, maxHops, batchSize int) error {
	if w.prov == nil {
		return nil
	}
	p, err := w.prov(ctx)
	if err != nil || p == nil {
		return fmt.Errorf("wiki: reconcile: provider: %w", err)
	}

	var seeds []string
	seeds = append(seeds, in.TouchedPages...)
	seeds = append(seeds, in.ArchivedPoints...)
	if len(seeds) == 0 {
		return nil
	}
	related, err := w.collectRelated(ctx, agentID, seeds, maxHops)
	if err != nil {
		return fmt.Errorf("wiki: reconcile: collect: %w", err)
	}
	if len(related) == 0 {
		return nil
	}

	if maxHops <= 0 {
		maxHops = 3
	}
	if batchSize <= 0 {
		batchSize = 10
	}
	tools := []types.Tool{NewQueryTool(w.deps, agentID), NewWriteTool(w.deps, agentID)}
	for start := 0; start < len(related); start += batchSize {
		end := start + batchSize
		if end > len(related) {
			end = len(related)
		}
		prompt := buildReconcilePrompt(in, related[start:end])
		loopCtx, cancel := context.WithTimeout(ctx, w.timeout)
		err := w.runLoop(loopCtx, p, reconcileSystem, tools, types.Message{Role: types.RoleUser, Text: prompt}, func(types.Event) {})
		cancel()
		if err != nil {
			return fmt.Errorf("wiki: reconcile: batch %d: %w", start/batchSize, err)
		}
	}
	return nil
}

// collectRelated BFS-walks the adjacency graph (outlinks/backlinks/has_page)
// from the seed ids up to maxHops deep, returning the related pages. Interest
// point ids (e.g. archived points) resolve to their concept pages via
// has_page edges.
func (w *Writer) collectRelated(ctx context.Context, agentID string, seeds []string, maxHops int) ([]store.Page, error) {
	if maxHops <= 0 {
		maxHops = 3
	}
	visited := map[string]bool{}
	var pages []store.Page
	queue := seeds
	// hop 0 = seeds themselves; each further hop walks one more level of the
	// graph. maxHops is the propagation depth (≤3 by default).
	for hop := 0; hop <= maxHops && len(queue) > 0; hop++ {
		var next []string
		for _, id := range queue {
			if visited[id] {
				continue
			}
			visited[id] = true
			p, err := w.deps.Store.GetPage(ctx, agentID, id)
			if err != nil {
				continue
			}
			if p == nil {
				// Likely an interest point id — resolve concept pages via has_page.
				outs, err := w.deps.Store.Outlinks(ctx, agentID, id)
				if err != nil {
					continue
				}
				for _, e := range outs {
					if e.Kind == store.EdgeHasPage && !visited[e.TargetID] {
						next = append(next, e.TargetID)
					}
				}
				continue
			}
			pages = append(pages, *p)
			outs, _ := w.deps.Store.Outlinks(ctx, agentID, id)
			for _, e := range outs {
				if !visited[e.TargetID] {
					next = append(next, e.TargetID)
				}
			}
			ins, _ := w.deps.Store.Backlinks(ctx, agentID, id)
			for _, e := range ins {
				if !visited[e.SourceID] {
					next = append(next, e.SourceID)
				}
			}
		}
		queue = next
	}
	return pages, nil
}

// buildReconcilePrompt renders the change summary and the related-page batch
// for the reconcile agent loop.
func buildReconcilePrompt(in ReconcileInput, batch []store.Page) string {
	var b strings.Builder
	b.WriteString("以下是本次结构性变更及需要协同处理的相关页面，请统一修改。\n\n")
	b.WriteString("## 本次变更\n")
	if len(in.TouchedPages) > 0 {
		b.WriteString(fmt.Sprintf("- 写入/更新页面: %s\n", strings.Join(in.TouchedPages, ", ")))
	}
	if len(in.ArchivedPoints) > 0 {
		b.WriteString(fmt.Sprintf("- 归档兴趣点（其概念页应标记 superseded）: %s\n", strings.Join(in.ArchivedPoints, ", ")))
	}
	b.WriteString("\n## 需要协同处理的相关页面\n")
	for i, p := range batch {
		b.WriteString(fmt.Sprintf("%d. [%s] %s (type=%s status=%s)\n", i+1, p.ID, p.Title, p.PageType, p.Status))
		if p.BodyMD != "" {
			b.WriteString(fmt.Sprintf("   Preview: %s\n", truncate(p.BodyMD, 300)))
		}
	}
	b.WriteString("\n请用 wiki_query 确认后，用 wiki_write 完成级联归档、矛盾闭环与内容协同。\n")
	return b.String()
}
