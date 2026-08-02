package store

import (
	"context"
	"fmt"
	"time"
)

// logRetain tracks the optional per-agent cap on retained log entries
// (0 = unlimited). logRetainDefault is the global default applied to agents
// without an explicit cap. Guarded by the store's writeMu.
var (
	logRetain        = map[string]int{}
	logRetainDefault int
)

// AppendLog records one structural change. When a per-agent retain cap is
// set (SetLogRetain > 0), the oldest entries beyond the cap are pruned.
func (s *SQLiteStore) AppendLog(ctx context.Context, l ChangeLog) error {
	lock := s.agentLock(l.AgentID)
	lock.Lock()
	defer lock.Unlock()
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now().UTC()
	}
	if l.ID == "" {
		l.ID = fmt.Sprintf("log-%d", time.Now().UnixNano())
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO change_log (id, agent_id, created_at, entity_kind, entity_id, title, action, edges)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, agent_id) DO UPDATE SET
			created_at = excluded.created_at,
			entity_kind = excluded.entity_kind,
			entity_id = excluded.entity_id,
			title = excluded.title,
			action = excluded.action,
			edges = excluded.edges`,
		l.ID, l.AgentID, l.CreatedAt, l.EntityKind, l.EntityID, l.Title, l.Action, marshalJSON(l.Edges)); err != nil {
		return fmt.Errorf("store: append log: %w", err)
	}
	s.pruneLogsLocked(ctx, l.AgentID)
	return nil
}

// ListLogs returns change logs for an agent, newest first, with pagination.
// limit<=0 means no limit; offset starts from 0.
func (s *SQLiteStore) ListLogs(ctx context.Context, agentID string, limit, offset int) ([]ChangeLog, error) {
	if offset < 0 {
		offset = 0
	}
	q := `SELECT id, agent_id, created_at, entity_kind, entity_id, title, action, edges
		FROM change_log WHERE agent_id = ? ORDER BY created_at DESC`
	var args []any
	args = append(args, agentID)
	if limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list logs: %w", err)
	}
	defer rows.Close()
	var out []ChangeLog
	for rows.Next() {
		var l ChangeLog
		var edges string
		if err := rows.Scan(&l.ID, &l.AgentID, &l.CreatedAt, &l.EntityKind, &l.EntityID, &l.Title, &l.Action, &edges); err != nil {
			return nil, err
		}
		unmarshalJSON(edges, &l.Edges)
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetLogRetain sets the per-agent retention cap (0 = unlimited). Older
// entries beyond the cap are pruned on the next AppendLog.
func (s *SQLiteStore) SetLogRetain(ctx context.Context, agentID string, n int) error {
	s.writeMu.Lock()
	if n <= 0 {
		delete(logRetain, agentID)
	} else {
		logRetain[agentID] = n
	}
	s.writeMu.Unlock()
	lock := s.agentLock(agentID)
	lock.Lock()
	defer lock.Unlock()
	return s.pruneLogsLocked(ctx, agentID)
}

// SetLogRetainDefault sets the global default retention cap applied to
// agents without an explicit per-agent cap (0 = unlimited).
func (s *SQLiteStore) SetLogRetainDefault(ctx context.Context, n int) error {
	s.writeMu.Lock()
	logRetainDefault = n
	s.writeMu.Unlock()
	if n <= 0 {
		return nil
	}
	// Prune all agents using the default cap.
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT agent_id FROM change_log`)
	if err != nil {
		return err
	}
	var agents []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return err
		}
		agents = append(agents, a)
	}
	rows.Close()
	for _, a := range agents {
		lock := s.agentLock(a)
		lock.Lock()
		err := s.pruneLogsLocked(ctx, a)
		lock.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// pruneLogsLocked deletes the oldest entries beyond the retention cap
// (per-agent cap, falling back to the global default). Caller must hold the
// agent write lock.
func (s *SQLiteStore) pruneLogsLocked(ctx context.Context, agentID string) error {
	s.writeMu.Lock()
	cap := logRetain[agentID]
	if cap <= 0 {
		cap = logRetainDefault
	}
	s.writeMu.Unlock()
	if cap <= 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM change_log
		WHERE agent_id = ? AND id IN (
			SELECT id FROM change_log WHERE agent_id = ?
			ORDER BY created_at DESC LIMIT -1 OFFSET ?
		)`, agentID, agentID, cap); err != nil {
		return fmt.Errorf("store: prune logs: %w", err)
	}
	return nil
}
