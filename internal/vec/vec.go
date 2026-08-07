package vec

import "context"

// Entry is one vector record to index.
type Entry struct {
	ID       string // page or interest-point id
	AgentID  string // namespace
	Kind     string // "interest_point" | "wiki_page"
	Vector   []float32
	Metadata map[string]string // title, page_type, etc.
}

// Hit is a search result.
type Hit struct {
	ID      string
	AgentID string
	Kind    string
	Score   float32 // cosine similarity / relevance, higher is better
	Meta    map[string]string
}

// VectorIndex is the pluggable vector store interface. Implementations:
//   - SQLiteVec: sqlite-vec vec0 virtual table over the shared *sql.DB
//   - Fallback: keyword-only search (plain metadata table + LIKE) when
//     sqlite-vec is unavailable
type VectorIndex interface {
	// Upsert inserts or replaces an entry's vector.
	Upsert(ctx context.Context, e Entry) error
	// Search returns the top-K most similar entries for a query vector.
	Search(ctx context.Context, agentID string, q []float32, topK int) ([]Hit, error)
	// SearchByKeywords falls back to keyword matching (used when vectors
	// are unavailable or query has no meaningful embedding).
	SearchByKeywords(ctx context.Context, agentID, query string, topK int) ([]Hit, error)
	// Delete removes an entry by id.
	Delete(ctx context.Context, agentID, id string) error
	// Available reports whether the index is usable at all (vector search
	// may still be a keyword-only degradation for the Fallback).
	Available() bool
	Close() error
}
