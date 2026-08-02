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

	// ---- adjacency edges (双链) ----
	UpsertEdge(ctx context.Context, agentID string, e Edge) error
	// AddEdgePair adds the edge and (for EdgeContradict) the reverse pair,
	// enforcing the EnsureContradictPair invariant.
	AddEdgePair(ctx context.Context, agentID string, e Edge) error
	Outlinks(ctx context.Context, agentID, sourceID string) ([]Edge, error)
	Backlinks(ctx context.Context, agentID, targetID string) ([]Edge, error)
	DeleteEdgesFor(ctx context.Context, agentID, sourceID string) error

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

	Close() error
}
