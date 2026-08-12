package store

import (
	"context"
	"fmt"
	"time"
)

// RecordPendingLink notes a [[target]] in page sourceID whose target page
// does not exist yet (dead link). Upsert semantics: repeated calls update
// the timestamp instead of duplicating.
func (s *SQLiteStore) RecordPendingLink(ctx context.Context, agentID, sourceID, target string) error {
	return s.RecordPendingLinks(ctx, agentID, sourceID, []string{target})
}

// RecordPendingLinks batch-records dead links for one source page.
func (s *SQLiteStore) RecordPendingLinks(ctx context.Context, agentID, sourceID string, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	lock := s.agentLock(agentID)
	lock.Lock()
	defer lock.Unlock()
	now := time.Now()
	for _, target := range targets {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO pending_links (agent_id, source_id, target, created_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(agent_id, source_id, target) DO UPDATE SET
				created_at = excluded.created_at`,
			agentID, sourceID, target, now); err != nil {
			return fmt.Errorf("store: record pending link: %w", err)
		}
	}
	return nil
}

// ClearPendingLink removes a resolved pending link (its target page now
// exists). No-op when absent.
func (s *SQLiteStore) ClearPendingLink(ctx context.Context, agentID, sourceID, target string) error {
	return s.ClearPendingLinks(ctx, agentID, sourceID, []string{target})
}

// ClearPendingLinks batch-removes resolved pending links for one source page.
func (s *SQLiteStore) ClearPendingLinks(ctx context.Context, agentID, sourceID string, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	lock := s.agentLock(agentID)
	lock.Lock()
	defer lock.Unlock()
	for _, target := range targets {
		if _, err := s.db.ExecContext(ctx, `
			DELETE FROM pending_links
			WHERE agent_id = ? AND source_id = ? AND target = ?`,
			agentID, sourceID, target); err != nil {
			return fmt.Errorf("store: clear pending link: %w", err)
		}
	}
	return nil
}

// DeletePendingLinksFor removes all pending links for a source page.
func (s *SQLiteStore) DeletePendingLinksFor(ctx context.Context, agentID, sourceID string) error {
	lock := s.agentLock(agentID)
	lock.Lock()
	defer lock.Unlock()
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM pending_links
		WHERE agent_id = ? AND source_id = ?`,
		agentID, sourceID)
	if err != nil {
		return fmt.Errorf("store: delete pending links for: %w", err)
	}
	return nil
}

// ListPendingLinks returns all dead-link records for an agent.
func (s *SQLiteStore) ListPendingLinks(ctx context.Context, agentID string) ([]PendingLink, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT agent_id, source_id, target, created_at
		FROM pending_links WHERE agent_id = ? ORDER BY created_at DESC`, agentID)
	if err != nil {
		return nil, fmt.Errorf("store: list pending links: %w", err)
	}
	defer rows.Close()
	var out []PendingLink
	for rows.Next() {
		var p PendingLink
		if err := rows.Scan(&p.AgentID, &p.SourceID, &p.Target, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
