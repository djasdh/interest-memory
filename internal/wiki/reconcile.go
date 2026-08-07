package wiki

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/djasdh/interest-memory/internal/store"

	"github.com/djasdh/my-agent-core/types"
)

// ReconcileInput lists what changed this pipeline run so the reconcile stage
// knows which pages to propagate changes to.
type ReconcileInput struct {
	TouchedPages   []string // wiki page ids written/updated this run
	ArchivedPoints []string // interest point ids archived (deleted/superseded) this run
}

// ArchivedInfo is an archived interest point with the detail the reconcile
// stage needs: its title, its outbound edges (original outlinks), and — when
// it was superseded rather than deleted — the live replacement id/title
// (replacement chain).
type ArchivedInfo struct {
	ID               string
	Title            string
	Outlinks         []store.Edge
	ReplacementID    string
	ReplacementTitle string
	Superseded       bool // true: replacement chain (sequel) exists; false: pure deletion
}

// reconcileSystem guides the unified reconcile agent loop: it propagates a
// structural change to related pages (cascade archive / contradiction closure
// / content sync). It runs WITHOUT review — the changes are executed directly.
const reconcileSystem = `You are a wiki co-editing assistant. A page or interest point just underwent a structural change; you must uniformly update the related wiki pages to keep the knowledge base self-consistent.
Rules:
- Use wiki_query to find related pages and wiki_write to modify them (status may be passed, e.g. superseded/archived)
- Cascade archive: pages superseded/overthrown by this change should be marked status=superseded (delete their references when needed); they are no longer current knowledge
- Replacement substitution: if this change is a "replacement", content in related pages that references the superseded old point should be silently updated to the successor (new point)'s latest facts
- Deletion cleanup: if this change is a "deletion", content in related pages that still references the archived old point should be removed or corrected to avoid stale knowledge
- Contradiction closure: if a related page directly contradicts the changed facts, correct it or mark its status
- Content sync: places in related pages that mention the changed content should be updated to the latest facts
- Do not create pages unrelated to this change
- Output concise, execute the edits directly`

// ReconcileRelated propagates a structural change to related wiki pages
// within maxHops hops (default 3). It collects the related-page subgraph
// (outlinks/backlinks/has_page), then dispatches batches of up to batchSize
// pages to a unified agent loop (wiki_query + wiki_write only — no review)
// that performs cascade archive / contradiction closure / content sync.
// Archived interest points are classified as superseded (has a sequel
// replacement link) or deleted (pure archive): the superseded ones get a
// code-level fallback substitution (marking the old concept page superseded
// and rewriting [[wikilinks]]) plus a prompt hint; deleted ones only surface
// their original outlinks in the prompt.
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

	// Resolve archived-point detail (title + original outlinks + replacement chain).
	archived := w.resolveArchived(ctx, agentID, in.ArchivedPoints)
	// Code-level fallback substitution for superseded points.
	if err := w.applyCodeFallback(ctx, agentID, archived); err != nil {
		return fmt.Errorf("wiki: reconcile: fallback: %w", err)
	}

	related, err := w.collectRelated(ctx, agentID, seeds, maxHops)
	if err != nil {
		return fmt.Errorf("wiki: reconcile: collect: %w", err)
	}
	// Nothing within 3 hops: empty or all archived → log silently, no LLM run.
	if len(related) == 0 || allArchived(related) {
		reason := "no related content within 3 hops"
		if len(related) > 0 {
			reason = "all related pages within 3 hops are already archived"
		}
		log.Printf("reconcile: silent skip %s (agent=%s archived=%d)", reason, agentID, len(archived))
		for _, a := range archived {
			log.Printf("reconcile: %s", describeArchived(a))
		}
		return nil
	}

	tools := []types.Tool{NewQueryTool(w.deps, agentID), NewWriteTool(w.deps, agentID)}
	for start := 0; start < len(related); start += batchSize {
		end := start + batchSize
		if end > len(related) {
			end = len(related)
		}
		prompt := buildReconcilePrompt(in, archived, related[start:end], w.lang)
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

// applyCodeFallback performs the code-level silent substitution for superseded
// archived points: marks the old concept page superseded and rewrites backlink
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
	parts := []string{fmt.Sprintf("archived %s", a.ID)}
	if a.Title != "" {
		parts = append(parts, fmt.Sprintf("%q", a.Title))
	}
	if a.Superseded {
		rep := a.ReplacementID
		if a.ReplacementTitle != "" {
			rep = fmt.Sprintf("%s (%q)", a.ReplacementID, a.ReplacementTitle)
		}
		parts = append(parts, "replacement chain: "+a.ID+" → "+rep)
	} else {
		outs := make([]string, 0, len(a.Outlinks))
		for _, e := range a.Outlinks {
			outs = append(outs, fmt.Sprintf("%s→%s", e.Kind, e.TargetID))
		}
		parts = append(parts, "original outlinks: ["+strings.Join(outs, ", ")+"]")
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
// agent loop. lang is the output language instruction appended at the end.
func buildReconcilePrompt(in ReconcileInput, archived []ArchivedInfo, batch []store.Page, lang string) string {
	if lang == "" {
		lang = "English"
	}
	var b strings.Builder
	b.WriteString("The following structural change happened and the related pages below need coordinated updates. Apply the edits uniformly.\n\n")
	b.WriteString("## This change\n")
	if len(in.TouchedPages) > 0 {
		b.WriteString(fmt.Sprintf("- Pages written/updated: %s\n", strings.Join(in.TouchedPages, ", ")))
	}
	for _, a := range archived {
		if a.Superseded {
			rep := a.ReplacementID
			if a.ReplacementTitle != "" {
				rep = fmt.Sprintf("%s (%s)", a.ReplacementID, a.ReplacementTitle)
			}
			b.WriteString(fmt.Sprintf("- Replacement: archived interest point %s (%s); the replacement chain is %s → %s, so content in related pages referencing the old point should be silently replaced with the new point\n",
				a.ID, a.Title, a.ID, rep))
		} else {
			outs := make([]string, 0, len(a.Outlinks))
			for _, e := range a.Outlinks {
				outs = append(outs, fmt.Sprintf("%s→%s", e.Kind, e.TargetID))
			}
			b.WriteString(fmt.Sprintf("- Deletion: archived interest point %s (%s); original outlinks: [%s]; content in related pages referencing it should be removed or corrected\n",
				a.ID, a.Title, strings.Join(outs, ", ")))
		}
	}
	b.WriteString("\n## Related pages to update\n")
	for i, p := range batch {
		b.WriteString(fmt.Sprintf("%d. [%s] %s (type=%s status=%s)\n", i+1, p.ID, p.Title, p.PageType, p.Status))
		if p.BodyMD != "" {
			b.WriteString(fmt.Sprintf("   Preview: %s\n", truncate(p.BodyMD, 300)))
		}
	}
	b.WriteString("\nConfirm with wiki_query, then use wiki_write to complete cascade archive, replacement substitution, contradiction closure, and content sync.\n")
	b.WriteString(fmt.Sprintf("Write all page content (title, body, related links) in '%s'.\n", lang))
	return b.String()
}
