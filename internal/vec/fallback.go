package vec

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
)

// Fallback implements VectorIndex with keyword-only search (no sqlite-vec).
// It mirrors the vec metadata in a plain table and searches with LIKE.
// This is the degradation path when sqlite-vec is unavailable.
type Fallback struct {
	db    *sql.DB
	mu    sync.Mutex
	ready bool
}

// NewFallback creates the keyword fallback store.
func NewFallback(db *sql.DB) (*Fallback, error) {
	f := &Fallback{db: db}
	if err := f.ensureTable(context.Background()); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *Fallback) ensureTable(ctx context.Context) error {
	_, err := f.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS vec_meta (
		id TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		body TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (id, agent_id, kind)
	)`)
	return err
}

// Available always reports true: the fallback is usable, but only for
// keyword search, never real vector search.
func (f *Fallback) Available() bool { return true }

// Upsert stores the metadata (vector is ignored in fallback mode).
func (f *Fallback) Upsert(ctx context.Context, e Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	title := e.Metadata["title"]
	body := e.Metadata["body"]
	kind := e.Kind
	if kind == "" {
		kind = "wiki_page"
	}
	if err := f.ensureTable(ctx); err != nil {
		return err
	}
	_, err := f.db.ExecContext(ctx, `INSERT OR REPLACE INTO vec_meta (id, agent_id, kind, title, body)
		VALUES (?, ?, ?, ?, ?)`, e.ID, e.AgentID, kind, title, body)
	return err
}

func (f *Fallback) Search(ctx context.Context, agentID string, q []float32, topK int) ([]Hit, error) {
	return nil, nil
}

func (f *Fallback) SearchByKeywords(ctx context.Context, agentID, query string, topK int) ([]Hit, error) {
	if topK <= 0 {
		topK = 8
	}
	pattern := "%" + strings.ToLower(query) + "%"
	rows, err := f.db.QueryContext(ctx, `
		SELECT id, agent_id, kind, title FROM vec_meta
		WHERE agent_id = ? AND (lower(title) LIKE ? OR lower(body) LIKE ?)
		LIMIT ?`, agentID, pattern, pattern, topK)
	if err != nil {
		return nil, fmt.Errorf("vec: fallback search: %w", err)
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		var title string
		if err := rows.Scan(&h.ID, &h.AgentID, &h.Kind, &title); err != nil {
			return nil, err
		}
		h.Score = 0.5 // keyword match, neutral score
		h.Meta = map[string]string{"title": title}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (f *Fallback) Delete(ctx context.Context, agentID, id string) error {
	_, err := f.db.ExecContext(ctx, `DELETE FROM vec_meta WHERE id = ? AND agent_id = ?`, id, agentID)
	return err
}

func (f *Fallback) Close() error { return nil }

var _ VectorIndex = (*Fallback)(nil)
