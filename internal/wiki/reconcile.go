package wiki

import (
	"context"
	"fmt"
	"log"
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

// ArchivedInfo is an archived interest point with the detail the reconcile
// stage needs: its title, its outbound edges (原本出边), and — when it was
// superseded rather than deleted — the live replacement id/title (替代链路).
type ArchivedInfo struct {
	ID               string
	Title            string
	Outlinks         []store.Edge
	ReplacementID    string
	ReplacementTitle string
	Superseded       bool // true: 替代链路 (sequel) 存在；false: 纯删除
}

// reconcileSystem guides the unified reconcile agent loop: it propagates a
// structural change to related pages (级联归档 / 矛盾闭环 / 内容协同). It runs
// WITHOUT review — the changes are executed directly.
const reconcileSystem = `你是一个 wiki 协同编辑助手。某个页面或兴趣点刚刚发生了结构性变更，你需要统一修改与其相关的 wiki 页面，保持知识库自洽。
规则：
- 用 wiki_query 查询相关页，用 wiki_write 修改（可传 status 参数，如 superseded/archived）
- 级联归档：被取代/推翻的旧页应标记 status=superseded（必要时删除其引用），不再作为当前知识
- 替代替换：若本次变更是"替代"，相关页中引用被替代旧点的内容应静默更新为替代者（新点）的最新事实
- 删除清理：若本次变更是"删除"，相关页中仍引用已归档旧点的内容应移除或修正，避免残留失效知识
- 矛盾闭环：若某相关页与变更后的事实直接矛盾，修正或标记其状态
- 内容协同：相关页中提到已变化内容的地方应更新为最新事实
- 不要新增与本次变更无关的页面
- 输出简洁，直接执行修改`

// ReconcileRelated propagates a structural change to related wiki pages
// within maxHops hops (default 3). It collects the related-page subgraph
// (outlinks/backlinks/has_page), then dispatches batches of up to batchSize
// pages to a unified agent loop (wiki_query + wiki_write only — no review)
// that performs 级联归档 / 矛盾闭环 / 内容协同. Archived interest points are
// classified as superseded (有 sequel 替代链路) or deleted (纯归档): the
// superseded ones get a code-level fallback substitution (marking the old
// concept page superseded and rewriting [[wikilinks]]) plus a prompt hint;
// deleted ones only surface their 原本出边 in the prompt.
func (w *Writer) ReconcileRelated(ctx context.Context, agentID string, in ReconcileInput, maxHops, batchSize int) error {
	if maxHops <= 0 {
		maxHops = 3
	}
	if batchSize <= 0 {
		batchSize = 10
	}
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

	// Resolve archived-point detail (title + 原本出边 + 替代链路).
	archived := w.resolveArchived(ctx, agentID, in.ArchivedPoints)
	// Code-level fallback substitution for superseded points.
	if err := w.applyCodeFallback(ctx, agentID, archived); err != nil {
		return fmt.Errorf("wiki: reconcile: fallback: %w", err)
	}

	related, err := w.collectRelated(ctx, agentID, seeds, maxHops)
	if err != nil {
		return fmt.Errorf("wiki: reconcile: collect: %w", err)
	}
	// 三跳内无内容：为空或全部已归档 → 静默提示，不跑 LLM。
	if len(related) == 0 || allArchived(related) {
		reason := "三跳内无相关内容"
		if len(related) > 0 {
			reason = "三跳内相关页面均已归档"
		}
		log.Printf("归档补链: 静默提示 %s (agent=%s archived=%d)", reason, agentID, len(archived))
		for _, a := range archived {
			log.Printf("归档补链: %s", describeArchived(a))
		}
		return nil
	}

	tools := []types.Tool{NewQueryTool(w.deps, agentID), NewWriteTool(w.deps, agentID)}
	for start := 0; start < len(related); start += batchSize {
		end := start + batchSize
		if end > len(related) {
			end = len(related)
		}
		prompt := buildReconcilePrompt(in, archived, related[start:end])
		loopCtx, cancel := context.WithTimeout(ctx, w.timeout)
		err := w.runLoop(loopCtx, p, reconcileSystem, tools, types.Message{Role: types.RoleUser, Text: prompt}, func(types.Event) {})
		cancel()
		if err != nil {
			return fmt.Errorf("wiki: reconcile: batch %d: %w", start/batchSize, err)
		}
	}
	return nil
}

// resolveArchived loads each archived interest point's title and outlinks,
// and classifies supersede vs delete via the presence of a sequel outlink.
func (w *Writer) resolveArchived(ctx context.Context, agentID string, ids []string) []ArchivedInfo {
	out := make([]ArchivedInfo, 0, len(ids))
	for _, id := range ids {
		a := ArchivedInfo{ID: id}
		if p, err := w.deps.Store.GetInterestPoint(ctx, agentID, id); err == nil && p != nil {
			a.Title = p.Name
		}
		if outs, err := w.deps.Store.Outlinks(ctx, agentID, id); err == nil {
			a.Outlinks = outs
			for _, e := range outs {
				if e.Kind == store.EdgeSequel {
					a.Superseded = true
					a.ReplacementID = e.TargetID
					if rp, err := w.deps.Store.GetInterestPoint(ctx, agentID, e.TargetID); err == nil && rp != nil {
						a.ReplacementTitle = rp.Name
					}
					break
				}
			}
		}
		out = append(out, a)
	}
	return out
}

// applyCodeFallback performs the code-level 静默替换 for superseded archived
// points: marks the old concept page superseded and rewrites backlink
// wikilinks from the old page to its successor page.
func (w *Writer) applyCodeFallback(ctx context.Context, agentID string, archived []ArchivedInfo) error {
	for _, a := range archived {
		if !a.Superseded || a.ReplacementID == "" {
			continue
		}
		oldPageID := a.ID
		if p, err := w.deps.Store.GetPage(ctx, agentID, a.ID); err == nil && p != nil {
			oldPageID = p.ID
		} else {
			// Archived point id: resolve its concept page via has_page.
			outs, _ := w.deps.Store.Outlinks(ctx, agentID, a.ID)
			for _, e := range outs {
				if e.Kind == store.EdgeHasPage {
					oldPageID = e.TargetID
					break
				}
			}
		}
		newPageID := ""
		if outs, err := w.deps.Store.Outlinks(ctx, agentID, a.ReplacementID); err == nil {
			for _, e := range outs {
				if e.Kind == store.EdgeHasPage {
					newPageID = e.TargetID
					break
				}
			}
		}
		if oldPageID == "" || newPageID == "" || oldPageID == newPageID {
			continue
		}
		// Mark the old concept page superseded (best-effort).
		if p, err := w.deps.Store.GetPage(ctx, agentID, oldPageID); err == nil && p != nil {
			if p.Status == "" || p.Status == "active" {
				p.Status = "superseded"
				_ = w.deps.Store.UpsertPage(ctx, *p)
			}
		}
		// Rewrite backlink wikilinks pointing at the old page.
		ins, err := w.deps.Store.Backlinks(ctx, agentID, oldPageID)
		if err != nil {
			continue
		}
		for _, e := range ins {
			if e.Kind != store.EdgeReference && e.Kind != store.EdgeRelated && e.Kind != store.EdgeContradict {
				continue
			}
			src, err := w.deps.Store.GetPage(ctx, agentID, e.SourceID)
			if err != nil || src == nil {
				continue
			}
			body := strings.ReplaceAll(src.BodyMD, "[["+oldPageID+"]]", "[["+newPageID+"]]")
			if body != src.BodyMD {
				src.BodyMD = body
				_ = w.deps.Store.UpsertPage(ctx, *src)
			}
		}
	}
	return nil
}

// allArchived reports whether every related page is no longer active.
func allArchived(pages []store.Page) bool {
	for _, p := range pages {
		if p.Status == "" || p.Status == "active" {
			return false
		}
	}
	return true
}

// describeArchived renders one archived point for the stdout log.
func describeArchived(a ArchivedInfo) string {
	parts := []string{fmt.Sprintf("已归档 %s", a.ID)}
	if a.Title != "" {
		parts = append(parts, "「"+a.Title+"」")
	}
	if a.Superseded {
		rep := a.ReplacementID
		if a.ReplacementTitle != "" {
			rep = fmt.Sprintf("%s 「%s」", a.ReplacementID, a.ReplacementTitle)
		}
		parts = append(parts, "替代链路: "+a.ID+" → "+rep)
	} else {
		outs := make([]string, 0, len(a.Outlinks))
		for _, e := range a.Outlinks {
			outs = append(outs, fmt.Sprintf("%s→%s", e.Kind, e.TargetID))
		}
		parts = append(parts, "原本出边: ["+strings.Join(outs, ", ")+"]")
	}
	return strings.Join(parts, " ")
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

// buildReconcilePrompt renders the change summary (including archived-point
// replacement links / outlinks) and the related-page batch for the reconcile
// agent loop.
func buildReconcilePrompt(in ReconcileInput, archived []ArchivedInfo, batch []store.Page) string {
	var b strings.Builder
	b.WriteString("以下是本次结构性变更及需要协同处理的相关页面，请统一修改。\n\n")
	b.WriteString("## 本次变更\n")
	if len(in.TouchedPages) > 0 {
		b.WriteString(fmt.Sprintf("- 写入/更新页面: %s\n", strings.Join(in.TouchedPages, ", ")))
	}
	for _, a := range archived {
		if a.Superseded {
			rep := a.ReplacementID
			if a.ReplacementTitle != "" {
				rep = fmt.Sprintf("%s（%s）", a.ReplacementID, a.ReplacementTitle)
			}
			b.WriteString(fmt.Sprintf("- 替代: 已归档兴趣点 %s（%s），替代链路为 %s → %s，相关页面中引用旧点的内容应静默替换为新点\n",
				a.ID, a.Title, a.ID, rep))
		} else {
			outs := make([]string, 0, len(a.Outlinks))
			for _, e := range a.Outlinks {
				outs = append(outs, fmt.Sprintf("%s→%s", e.Kind, e.TargetID))
			}
			b.WriteString(fmt.Sprintf("- 删除: 已归档兴趣点 %s（%s），原本出边: [%s]，相关页面中引用它的内容应移除或修正\n",
				a.ID, a.Title, strings.Join(outs, ", ")))
		}
	}
	b.WriteString("\n## 需要协同处理的相关页面\n")
	for i, p := range batch {
		b.WriteString(fmt.Sprintf("%d. [%s] %s (type=%s status=%s)\n", i+1, p.ID, p.Title, p.PageType, p.Status))
		if p.BodyMD != "" {
			b.WriteString(fmt.Sprintf("   Preview: %s\n", truncate(p.BodyMD, 300)))
		}
	}
	b.WriteString("\n请用 wiki_query 确认后，用 wiki_write 完成级联归档、替代替换、矛盾闭环与内容协同。\n")
	return b.String()
}
