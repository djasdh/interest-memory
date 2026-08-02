package store

import (
	"context"
	"fmt"
)

// ListTags aggregates all tags used across an agent's wiki pages, deduplicated
// and ordered by usage count descending. Built from the page tags JSON column
// (single source of truth — no separate tag table).
func (s *SQLiteStore) ListTags(ctx context.Context, agentID string) ([]TagCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT value, COUNT(*) AS c
		FROM wiki_pages, json_each(wiki_pages.tags)
		WHERE agent_id = ? AND value <> ''
		GROUP BY value
		ORDER BY c DESC, value ASC`, agentID)
	if err != nil {
		return nil, fmt.Errorf("store: list tags: %w", err)
	}
	defer rows.Close()
	var out []TagCount
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}
