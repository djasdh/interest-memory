package interest

import (
	"context"

	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/vec"
)

// Embedder computes embeddings for candidate text (implemented by
// *llm.Embedder, which carries the T2 content-hash LRU cache).
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// VectorIndex is the recall surface for historical interest points
// (implemented by vec.SQLiteVec / vec.Fallback).
type VectorIndex interface {
	Search(ctx context.Context, agentID string, q []float32, topK int) ([]vec.Hit, error)
	// Get fetches the stored entry (including its raw embedding vector) for a
	// historical id, so s2 can recompute exact pairwise similarity instead of
	// trusting Search's ranking score.
	Get(ctx context.Context, agentID, id string) (*vec.Entry, error)
	Upsert(ctx context.Context, e vec.Entry) error
	Delete(ctx context.Context, agentID, id string) error
}

// Store is the persistence surface s2 clustering needs (implemented by
// *store.SQLiteStore).
type Store interface {
	GetInterestPoint(ctx context.Context, agentID, id string) (*store.InterestPoint, error)
	UpsertInterestPoint(ctx context.Context, p store.InterestPoint) error
	AddEdgePair(ctx context.Context, agentID string, e store.Edge) error
	AppendLog(ctx context.Context, l store.ChangeLog) error
}
