package store

import "time"

// EdgeType enumerates the five allowed edge kinds between wiki pages /
// interest points (merged from my-agent-core's 4 kinds + the has_page link).
type EdgeType string

const (
	EdgeRelated    EdgeType = "related"     // generic association
	EdgeContradict EdgeType = "contradicts" // contradiction (enforced bidirectionally)
	EdgeSequel     EdgeType = "sequel"      // sequence/evolution
	EdgeReference  EdgeType = "references"  // reference/source
	EdgeHasPage    EdgeType = "has_page"    // interest point → wiki page
)

// Reliability (trustworthiness) — carried by interest points/claims, written
// by the correction layer.
type Reliability struct {
	Confidence float64    `json:"confidence"` // 0~1
	Evidence   []Evidence `json:"evidence"`
	Status     string     `json:"status"` // supported | contested | unknown
}

// Freshness (recency) — carried by interest points/claims, maintained by the
// correction layer.
type Freshness struct {
	Level     string    `json:"level"` // fresh | aging | stale | unknown
	UpdatedAt time.Time `json:"updated_at"`
	TTLDays   int       `json:"ttl_days"`
}

// Evidence is one provenance record backing a claim, locatable to source
// (turn range in a session, or a web URL + the query that surfaced it).
type Evidence struct {
	Kind       string    `json:"kind"`                  // session | page | manual | web
	SourceID   string    `json:"source_id"`             // provenance: session_id / page_id / URL
	URL        string    `json:"url,omitempty"`         // source URL for web evidence
	TurnRange  [2]int    `json:"turn_range,omitempty"`  // in-session turn span (1-indexed, 0 = unknown)
	Query      string    `json:"query,omitempty"`       // the search query that surfaced this evidence
	CapturedAt time.Time `json:"captured_at,omitempty"` // when the evidence was captured
	Excerpt    string    `json:"excerpt"`
}

// InterestPoint is a first-class entity identified by fork analysis.
type InterestPoint struct {
	ID             string      `json:"id"`
	AgentID        string      `json:"agent_id"`
	Name           string      `json:"name"`
	Summary        string      `json:"summary"`
	Keywords       []string    `json:"keywords"`
	Importance     float64     `json:"importance"`
	Status         string      `json:"status"`     // active | archived
	Subjective     bool        `json:"subjective"` // subjective preference/opinion flag (exempt from web fact-check)
	TurnRange      [2]int      `json:"turn_range"` // source session's global turn range [start,end]
	EventTime      time.Time   `json:"event_time"` // event time (session start; session_date with received_at fallback)
	Reliability    Reliability `json:"reliability"`
	Freshness      Freshness   `json:"freshness"`
	FirstSeenAt    time.Time   `json:"first_seen_at"`
	LastSeenAt     time.Time   `json:"last_seen_at"`
	SeenCount      int         `json:"seen_count"`
	SourceSessions []string    `json:"source_session_ids"`
	// WikiWorthy is the LLM's verdict (selective mode) on whether this point
	// deserves its own wiki page. nil = not judged (treated as worthy).
	WikiWorthy *bool `json:"wiki_worthy,omitempty"`
}

// PageType enumerates wiki page kinds.
type PageType string

const (
	PageEntity    PageType = "entity"
	PageConcept   PageType = "concept"
	PageSynthesis PageType = "synthesis"
	PageSource    PageType = "source"
)

// Page is a wiki page in the store.
type Page struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	PageType  PageType  `json:"page_type"`
	Title     string    `json:"title"`
	BodyMD    string    `json:"body_md"`           // markdown, may contain [[wikilink]]
	Status    string    `json:"status"`            // active | superseded | archived ("" treated as active)
	Tags      []string  `json:"tags,omitempty"`    // taxonomy tags
	Sources   []string  `json:"sources,omitempty"` // sources: web page URLs or existing page ids (may be empty for subjective points)
	EventTime time.Time `json:"event_time"`        // event time (session start)
	Claims    []Claim   `json:"claims"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TagCount is one aggregated tag with its usage count.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// PendingLink records a [[target]] wikilink in page sourceID whose target
// page does not exist yet (dead link). Used by RebuildEdges as a feedback
// loop so dead links are visible instead of silently dropped.
type PendingLink struct {
	AgentID   string    `json:"agent_id"`
	SourceID  string    `json:"source_id"`
	Target    string    `json:"target"`
	CreatedAt time.Time `json:"created_at"`
}

// Claim is a structured belief with evidence (core of the correction layer).
type Claim struct {
	ID         string     `json:"id"`
	AgentID    string     `json:"agent_id"`
	PageID     string     `json:"page_id"`
	Text       string     `json:"text"`
	Status     string     `json:"status"` // supported | contested | stale
	Confidence float64    `json:"confidence"`
	Evidence   []Evidence `json:"evidence"`
	Freshness  Freshness  `json:"freshness"`
}

// Edge is one directed relation in the adjacency table.
type Edge struct {
	SourceID  string    `json:"source_id"`
	TargetID  string    `json:"target_id"`
	Kind      EdgeType  `json:"kind"`
	Weight    float64   `json:"weight"`
	CreatedAt time.Time `json:"created_at"`
}

// Contradiction records a detected contradiction pair.
type Contradiction struct {
	ID          string    `json:"id"`
	AgentID     string    `json:"agent_id"`
	LeftID      string    `json:"left_id"`
	RightID     string    `json:"right_id"`
	Description string    `json:"description"`
	Status      string    `json:"status"` // open | resolved
	CreatedAt   time.Time `json:"created_at"`
}

// Transcript is a full conversation pushed at session end.
type Transcript struct {
	SessionID   string     `json:"session_id"`
	AgentID     string     `json:"agent_id"`
	TurnCount   int        `json:"turn_count"`
	RawTurns    string     `json:"raw_turns"`              // JSON: [{role, content}]
	ReceivedAt  time.Time  `json:"received_at"`            // server receive time
	SessionDate *time.Time `json:"session_date,omitempty"` // passed-through session start time (RFC3339, nullable)
	ProcessedAt *time.Time `json:"processed_at,omitempty"`
}

// LogEdge is one structural edge change (add/remove) recorded in a ChangeLog.
// Only structural kinds (has_page/related/contradicts/sequel) are logged;
// references (wikilink rebuild diff) is excluded.
type LogEdge struct {
	Action   string   `json:"action"` // add | remove
	SourceID string   `json:"source_id"`
	TargetID string   `json:"target_id"`
	Kind     EdgeType `json:"kind"`
	Weight   float64  `json:"weight"`
}

// ChangeLog records one structural change: entity title, action, and the
// structural edges touched. No LLM-generated reason is stored.
type ChangeLog struct {
	ID         string    `json:"id"`
	AgentID    string    `json:"agent_id"`
	CreatedAt  time.Time `json:"created_at"`
	EntityKind string    `json:"entity_kind"` // interest_point | wiki_page
	EntityID   string    `json:"entity_id"`
	Title      string    `json:"title"`
	Action     string    `json:"action"` // create | update | merge | archive | supersede | edge_change
	Edges      []LogEdge `json:"edges,omitempty"`
}
