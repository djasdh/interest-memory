package store

import "time"

// EdgeType enumerates the five allowed edge kinds between wiki pages /
// interest points (合并自 my-agent-core 的 4 种 + has_page 关联).
type EdgeType string

const (
	EdgeRelated    EdgeType = "related"     // 一般关联
	EdgeContradict EdgeType = "contradicts" // 矛盾（双向强制）
	EdgeSequel     EdgeType = "sequel"      // 序列/演进
	EdgeReference  EdgeType = "references"  // 引用/来源
	EdgeHasPage    EdgeType = "has_page"    // 兴趣点 → wiki 页
)

// Reliability（可用度）— 贯穿兴趣点/claims，纠错层写入。
type Reliability struct {
	Confidence float64    `json:"confidence"` // 0~1
	Evidence   []Evidence `json:"evidence"`
	Status     string     `json:"status"` // supported | contested | unknown
}

// Freshness（时效度）— 贯穿兴趣点/claims，纠错层维护。
type Freshness struct {
	Level     string    `json:"level"` // fresh | aging | stale | unknown
	UpdatedAt time.Time `json:"updated_at"`
	TTLDays   int       `json:"ttl_days"`
}

// Evidence is one provenance record backing a claim.
type Evidence struct {
	Kind     string `json:"kind"`      // session | page | manual | web
	SourceID string `json:"source_id"` // 追溯来源
	Excerpt  string `json:"excerpt"`
}

// InterestPoint is a first-class entity identified by fork analysis.
type InterestPoint struct {
	ID             string      `json:"id"`
	AgentID        string      `json:"agent_id"`
	Name           string      `json:"name"`
	Summary        string      `json:"summary"`
	Keywords       []string    `json:"keywords"`
	Importance     float64     `json:"importance"`
	Status         string      `json:"status"` // active | archived
	Reliability    Reliability `json:"reliability"`
	Freshness      Freshness   `json:"freshness"`
	FirstSeenAt    time.Time   `json:"first_seen_at"`
	LastSeenAt     time.Time   `json:"last_seen_at"`
	SeenCount      int         `json:"seen_count"`
	SourceSessions []string    `json:"source_session_ids"`
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
	BodyMD    string    `json:"body_md"` // markdown，含 [[wikilink]]
	Claims    []Claim   `json:"claims"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Claim is a structured belief with evidence (纠错层核心）。
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
	RawTurns    string     `json:"raw_turns"` // JSON: [{role, content}]
	ReceivedAt  time.Time  `json:"received_at"`
	ProcessedAt *time.Time `json:"processed_at,omitempty"`
}
