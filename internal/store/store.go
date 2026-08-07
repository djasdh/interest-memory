package store

import "context"

// Store is the facade interface for all persistent state, scoped by agent_id.
// Domain layers depend on this interface only (mock-friendly); the SQLite
// implementation lives in store.go.
type Store interface {
	// ---- interest points ----
	UpsertInterestPoint(ctx context.Context, p InterestPoint) error
	GetInterestPoint(ctx context.Context, agentID, id string) (*InterestPoint, error)
	ListInterestPoints(ctx context.Context, agentID string) ([]InterestPoint, error)
	SearchInterestPointsByKeywords(ctx context.Context, agentID, query string, limit int) ([]InterestPoint, error)

	// ---- wiki pages ----
	UpsertPage(ctx context.Context, p Page) error
	GetPage(ctx context.Context, agentID, id string) (*Page, error)
	ListPages(ctx context.Context, agentID string, pageType PageType) ([]Page, error)
	SearchPagesByKeywords(ctx context.Context, agentID, query string, limit int) ([]Page, error)
	// ListTags aggregates page tags for the agent (tag taxonomy), count desc.
	ListTags(ctx context.Context, agentID string) ([]TagCount, error)

	// ---- adjacency edges (双链) ----
	UpsertEdge(ctx context.Context, agentID string, e Edge) error
	// AddEdgePair adds the edge and (for EdgeContradict) the reverse pair,
	// enforcing the EnsureContradictPair invariant.
	AddEdgePair(ctx context.Context, agentID string, e Edge) error
	Outlinks(ctx context.Context, agentID, sourceID string) ([]Edge, error)
	Backlinks(ctx context.Context, agentID, targetID string) ([]Edge, error)
	DeleteEdgesFor(ctx context.Context, agentID, sourceID string) error
	// ResolveReplacement returns the live successor of an archived/superseded
	// entity (or nil), following sequel edges. Used by recall/search to
	// silently substitute superseded entities.
	ResolveReplacement(ctx context.Context, agentID, id string) (*Replacement, error)

	// ---- pending links (死链反馈) ----
	// RecordPendingLink notes a [[target]] in page sourceID that has no
	// matching page yet (dead link). Upsert semantics: repeated calls for the
	// same pair update the timestamp, not duplicate.
	RecordPendingLink(ctx context.Context, agentID, sourceID, target string) error
	// ClearPendingLink removes a resolved pending link (its target page now
	// exists). No-op when absent.
	ClearPendingLink(ctx context.Context, agentID, sourceID, target string) error
	// DeletePendingLinksFor removes all pending links for a source page
	// (used by RebuildEdges to refresh the dead-link set from current body).
	DeletePendingLinksFor(ctx context.Context, agentID, sourceID string) error
	ListPendingLinks(ctx context.Context, agentID string) ([]PendingLink, error)

	// ---- claims ----
	UpsertClaim(ctx context.Context, c Claim) error
	ListClaims(ctx context.Context, agentID, pageID string) ([]Claim, error)

	// ---- contradictions ----
	UpsertContradiction(ctx context.Context, c Contradiction) error
	ListContradictions(ctx context.Context, agentID string) ([]Contradiction, error)

	// ---- transcripts ----
	SaveTranscript(ctx context.Context, t Transcript) error
	GetTranscript(ctx context.Context, agentID, sessionID string) (*Transcript, error)
	// ListUnprocessedTranscripts returns transcripts for agentID that have not
	// been marked processed (processed_at IS NULL), oldest first. Used by the
	// worker queue and manual fork to pull pending session-end data.
	ListUnprocessedTranscripts(ctx context.Context, agentID string) ([]Transcript, error)
	MarkTranscriptProcessed(ctx context.Context, agentID, sessionID string) error

	// ---- change log ----
	AppendLog(ctx context.Context, l ChangeLog) error
	ListLogs(ctx context.Context, agentID string, limit, offset int) ([]ChangeLog, error)
	SetLogRetain(ctx context.Context, agentID string, n int) error
	SetLogRetainDefault(ctx context.Context, n int) error

	// ListAgentIDs returns the distinct namespaces that have persisted data
	// (interest points or wiki pages). Used for the "all" namespace-sharing
	// mode to discover the full namespace set dynamically.
	ListAgentIDs(ctx context.Context) ([]string, error)

	Close() error
}
