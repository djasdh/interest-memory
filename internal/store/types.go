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

// Evidence is one provenance record backing a claim, locatable to source
// (turn range in a session, or a web URL + the query that surfaced it).
type Evidence struct {
	Kind       string    `json:"kind"`                  // session | page | manual | web
	SourceID   string    `json:"source_id"`             // 追溯来源：session_id / page_id / URL
	URL        string    `json:"url,omitempty"`         // 网络证据原文 URL
	TurnRange  [2]int    `json:"turn_range,omitempty"`  // 会话内起止轮（1-indexed，0 表示未知）
	Query      string    `json:"query,omitempty"`       // 触发该证据检索的 query
	CapturedAt time.Time `json:"captured_at,omitempty"` // 证据采集时间
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
	Status         string      `json:"status"` // active | archived
	Subjective     bool        `json:"subjective"` // 主观观点/偏好标记（豁免联网核查）
	TurnRange      [2]int      `json:"turn_range"` // 来源会话全局轮次 [start,end]
	EventTime      time.Time   `json:"event_time"` // 事件发生时间（会话开始时间，session_date 兜底 received_at）
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
	Status    string    `json:"status"` // active | superseded | archived（"" 视为 active）
	Tags      []string  `json:"tags,omitempty"`   // 分类法标签
	Sources   []string  `json:"sources,omitempty"` // 来源：网页 URL 或现有页 id（主观性豁免可空）
	EventTime time.Time `json:"event_time"` // 事件发生时间（会话开始时间）
	Claims    []Claim   `json:"claims"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TagCount is one aggregated tag with its usage count.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
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
	ReceivedAt  time.Time  `json:"received_at"`            // 服务端接收时间
	SessionDate *time.Time `json:"session_date,omitempty"` // 透传的会话开始时间（RFC3339，可空）
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
