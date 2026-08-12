package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// UpsertEdge writes a single directed edge (no invariant enforcement).
// Prefer AddEdgePair for contradiction edges so the reverse direction is
// created automatically.
func (s *SQLiteStore) UpsertEdge(ctx context.Context, agentID string, e Edge) error {
	lock := s.agentLock(agentID)
	lock.Lock()
	defer lock.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO edges (source_id, target_id, kind, weight, created_at, agent_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id, source_id, target_id, kind) DO UPDATE SET
			weight = excluded.weight`,
		e.SourceID, e.TargetID, string(e.Kind), e.Weight, time.Now(), agentID)
	if err != nil {
		return fmt.Errorf("store: upsert edge: %w", err)
	}
	return nil
}

// AddEdgePair writes an edge and, for EdgeContradict, automatically creates
// the reverse edge (A→contradicts B implies B→contradicts A), enforcing the
// EnsureContradictPair invariant from the design.
func (s *SQLiteStore) AddEdgePair(ctx context.Context, agentID string, e Edge) error {
	return s.AddEdgePairs(ctx, agentID, []Edge{e})
}

// AddEdgePairs writes many edges in one transaction, enforcing the same
// bidirectional invariant for EdgeContradict. The batch amortizes the per-edge
// transaction cost during adjacency rebuild.
func (s *SQLiteStore) AddEdgePairs(ctx context.Context, agentID string, edges []Edge) error {
	if len(edges) == 0 {
		return nil
	}
	lock := s.agentLock(agentID)
	lock.Lock()
	defer lock.Unlock()

	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin edge tx: %w", err)
	}
	defer tx.Rollback()

	for _, e := range edges {
		if err := insertEdgeTx(ctx, tx, agentID, e.SourceID, e.TargetID, e.Kind, e.Weight, now); err != nil {
			return fmt.Errorf("store: insert edge: %w", err)
		}
		if e.Kind == EdgeContradict {
			if err := insertEdgeTx(ctx, tx, agentID, e.TargetID, e.SourceID, EdgeContradict, e.Weight, now); err != nil {
				return fmt.Errorf("store: insert reverse edge: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit edge tx: %w", err)
	}
	return nil
}

// insertEdgeTx upserts one edge within an existing transaction.
func insertEdgeTx(ctx context.Context, tx *sql.Tx, agentID, sourceID, targetID string, kind EdgeType, weight float64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO edges (source_id, target_id, kind, weight, created_at, agent_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id, source_id, target_id, kind) DO UPDATE SET weight = excluded.weight`,
		sourceID, targetID, string(kind), weight, now, agentID); err != nil {
		return err
	}
	return nil
}

// Outlinks returns edges whose source is sourceID.
func (s *SQLiteStore) Outlinks(ctx context.Context, agentID, sourceID string) ([]Edge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_id, target_id, kind, weight, created_at
		FROM edges WHERE agent_id = ? AND source_id = ? ORDER BY weight DESC`, agentID, sourceID)
	if err != nil {
		return nil, fmt.Errorf("store: outlinks: %w", err)
	}
	defer rows.Close()
	return scanEdges(rows)
}

// Backlinks returns edges whose target is targetID (reverse lookup).
func (s *SQLiteStore) Backlinks(ctx context.Context, agentID, targetID string) ([]Edge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_id, target_id, kind, weight, created_at
		FROM edges WHERE agent_id = ? AND target_id = ? ORDER BY weight DESC`, agentID, targetID)
	if err != nil {
		return nil, fmt.Errorf("store: backlinks: %w", err)
	}
	defer rows.Close()
	return scanEdges(rows)
}

// DeleteEdgesFor removes all edges originating from sourceID.
func (s *SQLiteStore) DeleteEdgesFor(ctx context.Context, agentID, sourceID string) error {
	lock := s.agentLock(agentID)
	lock.Lock()
	defer lock.Unlock()
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM edges WHERE agent_id = ? AND source_id = ?`, agentID, sourceID); err != nil {
		return fmt.Errorf("store: delete edges: %w", err)
	}
	return nil
}

type edgeScanner interface {
	Scan(dest ...any) error
}

func scanEdges(rows edgeRows) ([]Edge, error) {
	var out []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.SourceID, &e.TargetID, &e.Kind, &e.Weight, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type edgeRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}
