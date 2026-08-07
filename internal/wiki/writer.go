package wiki

import (
	"context"
	"fmt"
	"strings"
	"time"

	"interest-memory/internal/llm"
	"interest-memory/internal/store"

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
	lang    string
	timeout time.Duration
	runLoop loopRunner
}

// NewWriter builds a Writer. deps carries store/vec/embedder/llm/searcher;
// prov builds the LLM provider lazily per call so tests can inject a fake.
// lang is the output language hint injected into the system prompt
// (default "English").
func NewWriter(deps ToolsDeps, prov ProviderFactory, lang string) *Writer {
	if lang == "" {
		lang = "English"
	}
	system := `You are a wiki editing assistant. You write or update ONE wiki page per interest point.
Workflow (mandatory):
1. Use wiki_query to check whether a related page already exists; if so update it by its id instead of creating a new one
2. Write the page content (markdown, may use [[wikilink]] double links to related pages)
3. For objective factual claims, run verify_claims web fact-check before writing
4. Before the formal write, you MUST call the review tool to review your draft and adopt sound suggestions
5. Use wiki_write with page_type=concept, pass interest_point_id (this point's id), and provide suitable tags, edges, claims
6. Output concise and accurate content; do not write filler

Language: all page content (title, body, related links) must be written in ` + lang + `.
English proper nouns in titles (project names, tool names) may keep their original form, but explanatory text must use the configured language.

Link rules (mandatory):
- [[wikilink]] targets must be ids of pages that exist (as confirmed via wiki_query), e.g. [[pipeline-vs-agent-loop]];
  do NOT link tool names (e.g. [[verify_claims]]), external concepts (e.g. [[PostgreSQL]]), abstract tags (e.g. [[design-decision]]), or pages that do not exist in this wiki.
- If a related concept has no page in this wiki, do not link it with [[]]; mention it in plain text in the body.`
	return &Writer{
		deps:    deps,
		prov:    prov,
		system:  system,
		lang:    lang,
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
		NewVerifyClaimsTool(w.deps, w.lang),
		NewReviewTool(w.deps, agentID, w.lang),
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
		prompt := buildPointPrompt(ip, dialog, related, w.lang)

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
	b.WriteString("\nFollow the workflow: wiki_query → draft → verify_claims (objective claims) → review → wiki_write (with interest_point_id).\n")
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

// RebuildEdges regenerates the adjacency table from page body wikilinks.
// (Contradictions are persisted separately by service.persistContradictions,
// pipeline step 4.)
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
