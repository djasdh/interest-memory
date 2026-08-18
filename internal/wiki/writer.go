package wiki

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/djasdh/interest-memory/internal/llm"
	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/usage"

	"github.com/djasdh/my-agent-core/agent"
	"github.com/djasdh/my-agent-core/provider"
	"github.com/djasdh/my-agent-core/types"
)

// Compiler is the domain interface: write wiki pages from
// interest points + transcript via the agent loop, then rebuild edges.
// Compile returns the ids of pages touched this run (new/updated) so the
// caller can reconcile related pages.
type Compiler interface {
	Compile(ctx context.Context, agentID string, pts []store.InterestPoint, msgs []types.Message) ([]string, error)
	// RebuildEdges regenerates adjacency for the given touched page ids only
	// (incremental), unlike a full re-scan of every agent page.
	RebuildEdges(ctx context.Context, agentID string, touched []string) error
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
	deps         ToolsDeps
	prov         ProviderFactory
	system       string
	lang         string
	timeout      time.Duration
	runLoop      loopRunner
	tracker      *usage.Tracker
	verifyClaims bool
	groupSimVal  float64
}

// NewWriter builds a Writer. deps carries store/vec/embedder/llm/searcher;
// prov builds the LLM provider lazily per call so tests can inject a fake.
// lang is the output language hint injected into the system prompt
// (default "English").
func NewWriter(deps ToolsDeps, prov ProviderFactory, lang string, verifyClaims bool) *Writer {
	if lang == "" {
		lang = "English"
	}
	system := `You are a wiki editing assistant. You write or update wiki pages for interest points.
Workflow (mandatory):
1. Use wiki_query (or ip_query) to check whether a related page already exists; if so update it by its id instead of creating a new one
2. Write the page content (markdown, may use [[wikilink]] double links to related pages)
3. For objective factual claims, run verify_claims web fact-check before writing
4. Before the formal write, you MUST call the review tool to review your draft and adopt sound suggestions
5. Use wiki_write with page_type=concept, pass interest_point_ids (the interest point id(s) driving this page, as an array), and provide suitable tags, edges, claims
6. Output concise and accurate content; do not write filler

Language: all page content (title, body, related links) must be written in ` + lang + `.
English proper nouns in titles (project names, tool names) may keep their original form, but explanatory text must use the configured language.

Link rules (mandatory):
- [[wikilink]] targets must be ids of pages that exist (as confirmed via wiki_query), e.g. [[pipeline-vs-agent-loop]];
  do NOT link tool names (e.g. [[verify_claims]]), external concepts (e.g. [[PostgreSQL]]), abstract tags (e.g. [[design-decision]]), or pages that do not exist in this wiki.
- If a related concept has no page in this wiki, do not link it with [[]]; mention it in plain text in the body.`
	return &Writer{
		deps:         deps,
		prov:         prov,
		system:       system,
		lang:         lang,
		timeout:      10 * time.Minute,
		runLoop:      defaultRunLoop,
		verifyClaims: verifyClaims,
		groupSimVal:  0.75,
	}
}

// SetTracker wires an optional usage tracker; per-turn token usage from the
// agent loop is accumulated into it.
func (w *Writer) SetTracker(t *usage.Tracker) { w.tracker = t }

// SetGroupSim overrides the wikiloop clustering threshold (config wiki.group_sim).
func (w *Writer) SetGroupSim(sim float64) { w.groupSimVal = sim }

// GroupSim returns the current wikiloop clustering threshold.
func (w *Writer) GroupSim() float64 { return w.groupSimVal }

func defaultRunLoop(ctx context.Context, p *provider.Provider, system string, tools []types.Tool, prompt types.Message, emit types.EventSink) error {
	ag := agent.NewAgent(p)
	ag.SystemPrompt = system
	ag.Tools = tools
	_, err := ag.Prompt(prompt, nil, emit, ctx.Done())
	return err
}

// toolsFor builds the per-cluster tool set. The verify_claims (web fact-check)
// tool is omitted when verifyClaims is disabled so the model cannot trigger
// network search.
func (w *Writer) toolsFor(agentID string) []types.Tool {
	tools := []types.Tool{
		NewQueryTool(w.deps, agentID),
		NewIPQueryTool(w.deps, agentID),
		NewTagsTool(w.deps, agentID),
	}
	if w.verifyClaims {
		tools = append(tools, NewVerifyClaimsTool(w.deps, w.lang))
	}
	tools = append(tools,
		NewReviewTool(w.deps, agentID, w.lang),
		NewWriteTool(w.deps, agentID),
	)
	return tools
}

// maxCompileConcurrency bounds how many interest points the wiki stage writes
// in parallel. Writes stay safe because the store serializes per-agent writes;
// this only bounds the number of in-flight LLM agent loops (and thus peak
// request rate/cost).
const maxCompileConcurrency = 4

// Compile groups the interest points into EBD clusters (grouping only — never
// merges) and runs one wiki agent loop per cluster, plus one per isolated
// point, concurrently (bounded by maxCompileConcurrency). Each cluster's prompt
// presents all member points so the agent can decide a single-point page /
// multi-point shared page / update existing / merge into a non-group page.
// Returns the page ids touched this run.
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

	clusters, isolated, err := groupByCluster(ctx, w.deps, agentID, pts, w.groupSimVal)
	if err != nil {
		return nil, fmt.Errorf("wiki: group: %w", err)
	}

	units := make([][]store.InterestPoint, 0, len(clusters)+len(isolated))
	for _, c := range clusters {
		us := make([]store.InterestPoint, len(c))
		for i := range c {
			us[i] = c[i].Pt
		}
		units = append(units, us)
	}
	for _, iso := range isolated {
		units = append(units, []store.InterestPoint{iso.Pt})
	}

	tools := w.toolsFor(agentID)
	results := make([]compileResult, len(units))
	sem := make(chan struct{}, maxCompileConcurrency)
	var wg sync.WaitGroup
	for i, unit := range units {
		wg.Add(1)
		go func(i int, unit []store.InterestPoint) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = w.compileCluster(ctx, agentID, p, tools, unit, msgs)
		}(i, unit)
	}
	wg.Wait()

	var touched []string
	for _, r := range results {
		if r.err != nil {
			return touched, r.err
		}
		touched = append(touched, r.touched...)
	}
	return touched, nil
}

// compileResult is one cluster's wiki-write outcome.
type compileResult struct {
	touched []string
	err     error
}

// compileCluster runs one wikiloop for a cluster (or a single isolated point):
// the prompt presents all member interest points so the agent can decide a
// single-point page / multi-point shared page / update existing / merge into a
// non-group page. The cluster's points share one conversation segment span.
func (w *Writer) compileCluster(ctx context.Context, agentID string, p *provider.Provider, tools []types.Tool, unit []store.InterestPoint, msgs []types.Message) compileResult {
	related := prelookupRelated(ctx, w.deps, agentID, unit[0])
	dialog := dialogSpan(msgs, unit)
	prompt := buildClusterPrompt(unit, dialog, related, w.lang)

	var pointTouched []string
	loopCtx, cancel := context.WithTimeout(ctx, w.timeout)
	err := w.runLoop(loopCtx, p, w.system, tools, types.Message{Role: types.RoleUser, Text: prompt}, func(ev types.Event) {
		if ev.Type == "tool_execution_end" && ev.ToolName == "wiki_write" {
			if id, ok := ev.Args["id"].(string); ok && id != "" {
				pointTouched = append(pointTouched, id)
			}
		}
		if ev.Type == "turn_end" && ev.Message.Usage != nil && w.tracker != nil {
			w.tracker.Add(usage.Usage{
				Input:    int64(ev.Message.Usage.Input),
				Output:   int64(ev.Message.Usage.Output),
				CacheHit: int64(ev.Message.Usage.CacheRead),
			})
		}
	})
	cancel()
	if err != nil {
		return compileResult{err: fmt.Errorf("wiki: cluster %q: %w", unit[0].Name, err)}
	}
	// Backfill EventTime on pages the agent wrote without event_time, using the
	// cluster's earliest event time.
	var et time.Time
	for _, ip := range unit {
		if !ip.EventTime.IsZero() && (et.IsZero() || ip.EventTime.Before(et)) {
			et = ip.EventTime
		}
	}
	if !et.IsZero() {
		w.backfillEventTime(ctx, agentID, pointTouched, et)
	}
	w.logSkippedPoints(ctx, agentID, unit, pointTouched)
	return compileResult{touched: pointTouched}
}

// dialogSpan merges the TurnRange spans of all unit points into one continuous
// segment (min start, max end), clamped to msgs.
func dialogSpan(msgs []types.Message, unit []store.InterestPoint) string {
	if len(unit) == 0 {
		return ""
	}
	s, e := unit[0].TurnRange[0], unit[0].TurnRange[1]
	for _, ip := range unit[1:] {
		if ip.TurnRange[0] < s {
			s = ip.TurnRange[0]
		}
		if ip.TurnRange[1] > e {
			e = ip.TurnRange[1]
		}
	}
	return dialogSegment(msgs, [2]int{s, e})
}

// logSkippedPoints records which group points (worth a page) the agent loop
// did not attach to any written page, so misses can be revisited later
// (v2 §3.4 漏写兜底).
func (w *Writer) logSkippedPoints(ctx context.Context, agentID string, unit []store.InterestPoint, touched []string) {
	if len(touched) == 0 {
		return
	}
	touchedSet := map[string]bool{}
	for _, id := range touched {
		touchedSet[id] = true
	}
	pages, _ := w.deps.Store.InterestPointPages(ctx, agentID, idsOf(unit))
	covered := map[string]bool{}
	for _, r := range pages {
		if touchedSet[r.PageID] {
			covered[r.InterestPointID] = true
		}
	}
	for _, ip := range unit {
		if covered[ip.ID] {
			continue
		}
		if ip.WikiWorthy != nil && !*ip.WikiWorthy {
			continue
		}
		log.Printf("wiki: skipped write for interest point %q (%s) — not covered by pages %v", ip.Name, ip.ID, touched)
	}
}

func idsOf(pts []store.InterestPoint) []string {
	out := make([]string, len(pts))
	for i := range pts {
		out[i] = pts[i].ID
	}
	return out
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
// segment backing it, and the pre-looked-up related page summary. lang is the
// output language instruction appended at the end.
func buildPointPrompt(ip store.InterestPoint, dialog, related, lang string) string {
	if lang == "" {
		lang = "English"
	}
	var b strings.Builder
	b.WriteString("Write or update ONE wiki page for the following interest point.\n\n")
	b.WriteString("## Interest point\n")
	b.WriteString(fmt.Sprintf("- Topic: %s\n", ip.Name))
	if ip.Summary != "" {
		b.WriteString(fmt.Sprintf("- Summary: %s\n", truncate(ip.Summary, 500)))
	}
	if len(ip.Keywords) > 0 {
		b.WriteString(fmt.Sprintf("- Tags: %s\n", strings.Join(ip.Keywords, ", ")))
	}
	if ip.Subjective {
		b.WriteString("- Subjectivity: subjective (user's own preference/opinion) — no web fact-check needed\n")
	} else {
		b.WriteString("- Subjectivity: objective (factual claim) — run verify_claims web fact-check before writing\n")
	}
	if !ip.EventTime.IsZero() {
		b.WriteString(fmt.Sprintf("- Event time: %s (use this for wiki_write's event_time argument)\n", ip.EventTime.UTC().Format("2006-01-02T15:04:05Z07:00")))
	}
	if ip.Reliability.Status != "" {
		b.WriteString(fmt.Sprintf("- Reliability: %s (%.2f)\n", ip.Reliability.Status, ip.Reliability.Confidence))
	}
	if len(ip.Reliability.Evidence) > 0 {
		b.WriteString("\n## Evidence (cite sources when writing claims)\n")
		for _, e := range ip.Reliability.Evidence {
			loc := e.SourceID
			if e.URL != "" {
				loc = e.URL
			}
			b.WriteString(fmt.Sprintf("- [%s] %s\n", e.Kind, truncate(loc, 200)))
			if e.Excerpt != "" {
				b.WriteString(fmt.Sprintf("  Quote: %s\n", truncate(e.Excerpt, 200)))
			}
		}
	}
	if dialog != "" {
		b.WriteString("\n## Corresponding conversation segment (source turns)\n")
		b.WriteString(dialog)
	}
	if related != "" {
		b.WriteString("\n## Existing related pages (prefer updating rather than creating)\n")
		b.WriteString(related)
	}
	b.WriteString("\nFollow the workflow: wiki_query → draft → verify_claims (objective claims) → review → wiki_write (with interest_point_ids listing the interest point id(s) driving this page).\n")
	b.WriteString(fmt.Sprintf("Write all page content (title, body, related links) in '%s'.\n", lang))
	return b.String()
}

// buildClusterPrompt renders a clustered wikiloop prompt: it presents all
// member interest points (with wiki_worthy hints) so the agent can decide a
// single-point page / multi-point shared page / update existing / merge into a
// non-group page. A single-point cluster reuses buildPointPrompt.
func buildClusterPrompt(unit []store.InterestPoint, dialog, related, lang string) string {
	if len(unit) == 1 {
		return buildPointPrompt(unit[0], dialog, related, lang)
	}
	if lang == "" {
		lang = "English"
	}
	var b strings.Builder
	b.WriteString("Write or update wiki page(s) for the following interest point group.\n")
	b.WriteString("These points are related and may share one page, or each may deserve its own. Decide: (a) write one page per point; (b) write ONE page covering multiple points (declare all driving interest point ids in interest_point_ids); (c) update an existing page; (d) merge content into a page that already exists in the wiki (found via wiki_query). Points marked wiki_worthy=false are related context — they may contribute content or stay as reference only.\n\n")
	b.WriteString("## Interest points in this group\n")
	for i, ip := range unit {
		b.WriteString(fmt.Sprintf("%d. %s\n", i+1, ip.Name))
		if ip.Summary != "" {
			b.WriteString(fmt.Sprintf("   Summary: %s\n", truncate(ip.Summary, 300)))
		}
		if len(ip.Keywords) > 0 {
			b.WriteString(fmt.Sprintf("   Tags: %s\n", strings.Join(ip.Keywords, ", ")))
		}
		if ip.Reliability.Status != "" {
			b.WriteString(fmt.Sprintf("   Reliability: %s (%.2f)\n", ip.Reliability.Status, ip.Reliability.Confidence))
		}
		if ip.WikiWorthy != nil && !*ip.WikiWorthy {
			b.WriteString("   (wiki_worthy=false: related context, not required to have its own page)\n")
		}
		if !ip.EventTime.IsZero() {
			b.WriteString(fmt.Sprintf("   Event time: %s\n", ip.EventTime.UTC().Format("2006-01-02T15:04:05Z07:00")))
		}
		b.WriteString("\n")
	}
	var evidenceShown bool
	for _, ip := range unit {
		if len(ip.Reliability.Evidence) == 0 {
			continue
		}
		if !evidenceShown {
			b.WriteString("\n## Evidence (cite sources when writing claims)\n")
			evidenceShown = true
		}
		for _, e := range ip.Reliability.Evidence {
			loc := e.SourceID
			if e.URL != "" {
				loc = e.URL
			}
			b.WriteString(fmt.Sprintf("- [%s] %s\n", e.Kind, truncate(loc, 200)))
			if e.Excerpt != "" {
				b.WriteString(fmt.Sprintf("  Quote: %s\n", truncate(e.Excerpt, 200)))
			}
		}
	}
	if dialog != "" {
		b.WriteString("\n## Corresponding conversation segment (source turns)\n")
		b.WriteString(dialog)
	}
	if related != "" {
		b.WriteString("\n## Existing related pages (prefer updating rather than creating)\n")
		b.WriteString(related)
	}
	b.WriteString("\nWorkflow: ip_query → wiki_query → draft → verify_claims (objective claims) → review → wiki_write (with interest_point_ids listing the driving point id(s)).\n")
	b.WriteString(fmt.Sprintf("Write all page content (title, body, related links) in '%s'.\n", lang))
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

// RebuildEdges regenerates the adjacency table from the body wikilinks of the
// given touched pages (incremental: only pages written this run). It batches
// all store reads/writes so cost scales with the touched set, not the whole
// wiki. (Contradictions are persisted separately by
// service.persistContradictions, pipeline step 4.)
func (w *Writer) RebuildEdges(ctx context.Context, agentID string, touched []string) error {
	if len(touched) == 0 {
		return nil
	}
	pages, err := w.deps.Store.GetPagesByIDs(ctx, agentID, touched)
	if err != nil {
		return fmt.Errorf("wiki: rebuild: get pages: %w", err)
	}
	if len(pages) == 0 {
		return nil
	}

	// Collect every wikilink target from the touched pages (deduped).
	// Targets are normalized the same way wiki_write normalizes page ids
	// (lowercase, spaces/underscores -> '-'); otherwise a link written as
	// [[My Page]] would never match the stored id "my-page" and the
	// reference edge would be silently dropped into the pending set.
	type link struct{ source, target string }
	var links []link
	targetSeen := map[string]bool{}
	var targets []string
	for _, p := range pages {
		for _, raw := range ExtractWikilinks(p.BodyMD) {
			target := normalizeID(raw)
			if target == "" || target == p.ID {
				continue
			}
			links = append(links, link{p.ID, target})
			if !targetSeen[target] {
				targetSeen[target] = true
				targets = append(targets, target)
			}
		}
	}

	// Batch-resolve target existence in one query (instead of one GetPage per
	// link).
	exists := map[string]bool{}
	if len(targets) > 0 {
		existing, err := w.deps.Store.GetPagesByIDs(ctx, agentID, targets)
		if err != nil {
			return fmt.Errorf("wiki: rebuild: resolve targets: %w", err)
		}
		for _, pg := range existing {
			exists[pg.ID] = true
		}
	}

	// Clear stale out-edges and pending-link sets for the touched pages.
	for _, p := range pages {
		if err := w.deps.Store.DeleteEdgesFor(ctx, agentID, p.ID); err != nil {
			return fmt.Errorf("wiki: rebuild: delete edges: %w", err)
		}
		if err := w.deps.Store.DeletePendingLinksFor(ctx, agentID, p.ID); err != nil {
			return fmt.Errorf("wiki: rebuild: delete pending links: %w", err)
		}
	}

	// Partition links into resolved (edge) and dead (pending), then write each
	// group in one batch.
	var edges []store.Edge
	dead := map[string][]string{}
	resolved := map[string][]string{}
	for _, l := range links {
		if exists[l.target] {
			edges = append(edges, store.Edge{
				SourceID: l.source, TargetID: l.target,
				Kind: store.EdgeReference, Weight: 1, CreatedAt: timeNow(),
			})
			resolved[l.source] = append(resolved[l.source], l.target)
		} else {
			dead[l.source] = append(dead[l.source], l.target)
		}
	}
	if err := w.deps.Store.AddEdgePairs(ctx, agentID, edges); err != nil {
		return fmt.Errorf("wiki: rebuild: add edges: %w", err)
	}
	for source, targets := range dead {
		if err := w.deps.Store.RecordPendingLinks(ctx, agentID, source, targets); err != nil {
			return fmt.Errorf("wiki: rebuild: record pending: %w", err)
		}
	}
	for source, targets := range resolved {
		if err := w.deps.Store.ClearPendingLinks(ctx, agentID, source, targets); err != nil {
			return fmt.Errorf("wiki: rebuild: clear pending: %w", err)
		}
	}
	return nil
}

// Helper: exported for tests.
func timeNow() time.Time { return time.Now() }
