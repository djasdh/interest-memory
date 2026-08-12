package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ── interest points ───────────────────────────────────────────────────────

func (s *SQLiteStore) UpsertInterestPoint(ctx context.Context, p InterestPoint) error {
	lock := s.agentLock(p.AgentID)
	lock.Lock()
	defer lock.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO interest_points (
			id, agent_id, name, summary, keywords, importance, status, subjective,
			turn_range_start, turn_range_end, event_time,
			confidence, reliability_status, evidence, freshness_level, updated_at,
			ttl_days, first_seen_at, last_seen_at, seen_count, source_session_ids, wiki_worthy
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, agent_id) DO UPDATE SET
			name = excluded.name,
			summary = excluded.summary,
			keywords = excluded.keywords,
			importance = excluded.importance,
			status = excluded.status,
			subjective = excluded.subjective,
			turn_range_start = excluded.turn_range_start,
			turn_range_end = excluded.turn_range_end,
			event_time = excluded.event_time,
			confidence = excluded.confidence,
			reliability_status = excluded.reliability_status,
			evidence = excluded.evidence,
			freshness_level = excluded.freshness_level,
			updated_at = excluded.updated_at,
			ttl_days = excluded.ttl_days,
			last_seen_at = excluded.last_seen_at,
			seen_count = excluded.seen_count,
			source_session_ids = excluded.source_session_ids,
			wiki_worthy = excluded.wiki_worthy`,
		p.ID, p.AgentID, p.Name, p.Summary, marshalJSON(p.Keywords), p.Importance, p.Status,
		boolInt(p.Subjective), p.TurnRange[0], p.TurnRange[1], nullableTime(p.EventTime),
		p.Reliability.Confidence, p.Reliability.Status, marshalJSON(p.Reliability.Evidence),
		p.Freshness.Level, p.Freshness.UpdatedAt, p.Freshness.TTLDays,
		p.FirstSeenAt, p.LastSeenAt, p.SeenCount, marshalJSON(p.SourceSessions),
		nullableBool(p.WikiWorthy),
	)
	if err != nil {
		return fmt.Errorf("store: upsert interest point: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetInterestPoint(ctx context.Context, agentID, id string) (*InterestPoint, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, agent_id, name, summary, keywords, importance, status, subjective,
			turn_range_start, turn_range_end, event_time,
			confidence, reliability_status, evidence, freshness_level, updated_at,
			ttl_days, first_seen_at, last_seen_at, seen_count, source_session_ids, wiki_worthy
		FROM interest_points WHERE id = ? AND agent_id = ?`, id, agentID)
	p, err := scanInterestPoint(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get interest point: %w", err)
	}
	return p, nil
}

func (s *SQLiteStore) ListInterestPoints(ctx context.Context, agentID string) ([]InterestPoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent_id, name, summary, keywords, importance, status, subjective,
			turn_range_start, turn_range_end, event_time,
			confidence, reliability_status, evidence, freshness_level, updated_at,
			ttl_days, first_seen_at, last_seen_at, seen_count, source_session_ids, wiki_worthy
		FROM interest_points WHERE agent_id = ? ORDER BY importance DESC`, agentID)
	if err != nil {
		return nil, fmt.Errorf("store: list interest points: %w", err)
	}
	defer rows.Close()
	var out []InterestPoint
	for rows.Next() {
		p, err := scanInterestPoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetInterestPointsByIDs returns interest points for the given ids in one
// query. Missing ids are silently skipped; result order is unspecified.
func (s *SQLiteStore) GetInterestPointsByIDs(ctx context.Context, agentID string, ids []string) ([]InterestPoint, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, agentID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent_id, name, summary, keywords, importance, status, subjective,
			turn_range_start, turn_range_end, event_time,
			confidence, reliability_status, evidence, freshness_level, updated_at,
			ttl_days, first_seen_at, last_seen_at, seen_count, source_session_ids, wiki_worthy
		FROM interest_points WHERE agent_id = ? AND id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: get interest points by ids: %w", err)
	}
	defer rows.Close()
	var out []InterestPoint
	for rows.Next() {
		p, err := scanInterestPoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) SearchInterestPointsByKeywords(ctx context.Context, agentID, query string, limit int) ([]InterestPoint, error) {
	if query == "" || limit <= 0 {
		return nil, nil
	}
	pattern := "%" + strings.ToLower(query) + "%"
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent_id, name, summary, keywords, importance, status, subjective,
			turn_range_start, turn_range_end, event_time,
			confidence, reliability_status, evidence, freshness_level, updated_at,
			ttl_days, first_seen_at, last_seen_at, seen_count, source_session_ids, wiki_worthy
		FROM interest_points
		WHERE agent_id = ? AND (lower(name) LIKE ? OR lower(summary) LIKE ? OR lower(keywords) LIKE ?)
		ORDER BY importance DESC LIMIT ?`, agentID, pattern, pattern, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("store: search interest points: %w", err)
	}
	defer rows.Close()
	var out []InterestPoint
	for rows.Next() {
		p, err := scanInterestPoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanInterestPoint(row rowScanner) (*InterestPoint, error) {
	var p InterestPoint
	var keywords, evidence, sessions string
	var subjective, trStart, trEnd int
	var evt sql.NullTime
	var ww sql.NullInt64
	err := row.Scan(
		&p.ID, &p.AgentID, &p.Name, &p.Summary, &keywords, &p.Importance, &p.Status,
		&subjective, &trStart, &trEnd, &evt,
		&p.Reliability.Confidence, &p.Reliability.Status, &evidence, &p.Freshness.Level,
		&p.Freshness.UpdatedAt, &p.Freshness.TTLDays, &p.FirstSeenAt, &p.LastSeenAt,
		&p.SeenCount, &sessions, &ww,
	)
	if err != nil {
		return nil, err
	}
	unmarshalJSON(keywords, &p.Keywords)
	unmarshalJSON(evidence, &p.Reliability.Evidence)
	unmarshalJSON(sessions, &p.SourceSessions)
	p.Subjective = subjective != 0
	p.TurnRange = [2]int{trStart, trEnd}
	if evt.Valid {
		p.EventTime = evt.Time
	}
	if ww.Valid {
		b := ww.Int64 != 0
		p.WikiWorthy = &b
	}
	return &p, nil
}

// ── wiki pages ────────────────────────────────────────────────────────────

func (s *SQLiteStore) UpsertPage(ctx context.Context, p Page) error {
	lock := s.agentLock(p.AgentID)
	lock.Lock()
	defer lock.Unlock()
	status := p.Status
	if status == "" {
		status = "active"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO wiki_pages (id, agent_id, page_type, title, body_md, status, tags, sources, event_time, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, agent_id) DO UPDATE SET
			page_type = excluded.page_type,
			title = excluded.title,
			body_md = excluded.body_md,
			status = excluded.status,
			tags = excluded.tags,
			sources = excluded.sources,
			event_time = excluded.event_time,
			updated_at = excluded.updated_at`,
		p.ID, p.AgentID, string(p.PageType), p.Title, p.BodyMD, status, marshalJSON(p.Tags), marshalJSON(p.Sources), nullableTime(p.EventTime), p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: upsert page: %w", err)
	}
	return nil
}

// GetPage returns a single page WITHOUT its claims. Callers that need claims
// must fetch them explicitly via ListClaims (or ListClaimsForPageIDs for
// batches) — most readers (edge rebuild, wikilink resolution) only need the
// body/title, and eager claim loading made every page read an N+1.
func (s *SQLiteStore) GetPage(ctx context.Context, agentID, id string) (*Page, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, agent_id, page_type, title, body_md, status, tags, sources, event_time, created_at, updated_at
		FROM wiki_pages WHERE id = ? AND agent_id = ?`, id, agentID)
	var p Page
	var tags, sources string
	var evt sql.NullTime
	err := row.Scan(&p.ID, &p.AgentID, &p.PageType, &p.Title, &p.BodyMD, &p.Status, &tags, &sources, &evt, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get page: %w", err)
	}
	unmarshalJSON(tags, &p.Tags)
	unmarshalJSON(sources, &p.Sources)
	if evt.Valid {
		p.EventTime = evt.Time
	}
	return &p, nil
}

// GetPagesByIDs returns pages for the given ids in one query (no claims).
// Missing ids are silently skipped; result order is unspecified.
func (s *SQLiteStore) GetPagesByIDs(ctx context.Context, agentID string, ids []string) ([]Page, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, agentID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent_id, page_type, title, body_md, status, tags, sources, event_time, created_at, updated_at
		FROM wiki_pages WHERE agent_id = ? AND id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: get pages by ids: %w", err)
	}
	defer rows.Close()
	var out []Page
	for rows.Next() {
		var p Page
		var tags, sources string
		var evt sql.NullTime
		if err := rows.Scan(&p.ID, &p.AgentID, &p.PageType, &p.Title, &p.BodyMD, &p.Status, &tags, &sources, &evt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		unmarshalJSON(tags, &p.Tags)
		unmarshalJSON(sources, &p.Sources)
		if evt.Valid {
			p.EventTime = evt.Time
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ListPages(ctx context.Context, agentID string, pageType PageType) ([]Page, error) {
	var rows *sql.Rows
	var err error
	if pageType == "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, agent_id, page_type, title, body_md, status, tags, sources, event_time, created_at, updated_at
			FROM wiki_pages WHERE agent_id = ?`, agentID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, agent_id, page_type, title, body_md, status, tags, sources, event_time, created_at, updated_at
			FROM wiki_pages WHERE agent_id = ? AND page_type = ?`, agentID, string(pageType))
	}
	if err != nil {
		return nil, fmt.Errorf("store: list pages: %w", err)
	}
	defer rows.Close()
	var out []Page
	for rows.Next() {
		var p Page
		var tags, sources string
		var evt sql.NullTime
		if err := rows.Scan(&p.ID, &p.AgentID, &p.PageType, &p.Title, &p.BodyMD, &p.Status, &tags, &sources, &evt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		unmarshalJSON(tags, &p.Tags)
		unmarshalJSON(sources, &p.Sources)
		if evt.Valid {
			p.EventTime = evt.Time
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Claims are batch-filled in one query (instead of one per page). The rows
	// must be closed first: on a single connection the claim query would
	// otherwise block on the still-open scan.
	rows.Close()
	if len(out) > 0 {
		ids := make([]string, len(out))
		for i := range out {
			ids[i] = out[i].ID
		}
		if claims, err := s.ListClaimsForPageIDs(ctx, agentID, ids); err == nil {
			byPage := make(map[string][]Claim, len(out))
			for _, c := range claims {
				byPage[c.PageID] = append(byPage[c.PageID], c)
			}
			for i := range out {
				out[i].Claims = byPage[out[i].ID]
			}
		}
	}
	return out, nil
}

func (s *SQLiteStore) SearchPagesByKeywords(ctx context.Context, agentID, query string, limit int) ([]Page, error) {
	if query == "" || limit <= 0 {
		return nil, nil
	}
	pattern := "%" + strings.ToLower(query) + "%"
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent_id, page_type, title, body_md, status, tags, sources, event_time, created_at, updated_at
		FROM wiki_pages
		WHERE agent_id = ? AND (lower(title) LIKE ? OR lower(body_md) LIKE ?)
		ORDER BY updated_at DESC LIMIT ?`, agentID, pattern, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("store: search pages: %w", err)
	}
	defer rows.Close()
	var out []Page
	for rows.Next() {
		var p Page
		var tags, sources string
		var evt sql.NullTime
		if err := rows.Scan(&p.ID, &p.AgentID, &p.PageType, &p.Title, &p.BodyMD, &p.Status, &tags, &sources, &evt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		unmarshalJSON(tags, &p.Tags)
		unmarshalJSON(sources, &p.Sources)
		if evt.Valid {
			p.EventTime = evt.Time
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ── claims ────────────────────────────────────────────────────────────────

func (s *SQLiteStore) UpsertClaim(ctx context.Context, c Claim) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO claims (id, page_id, agent_id, text, status, confidence, evidence,
			freshness_level, updated_at, ttl_days)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, agent_id) DO UPDATE SET
			page_id = excluded.page_id,
			text = excluded.text,
			status = excluded.status,
			confidence = excluded.confidence,
			evidence = excluded.evidence,
			freshness_level = excluded.freshness_level,
			updated_at = excluded.updated_at,
			ttl_days = excluded.ttl_days`,
		c.ID, c.PageID, c.AgentID, c.Text, c.Status, c.Confidence,
		marshalJSON(c.Evidence), c.Freshness.Level, c.Freshness.UpdatedAt, c.Freshness.TTLDays)
	if err != nil {
		return fmt.Errorf("store: upsert claim: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListClaims(ctx context.Context, agentID, pageID string) ([]Claim, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, page_id, text, status, confidence, evidence, freshness_level, updated_at, ttl_days
		FROM claims WHERE page_id = ? AND agent_id = ?`, pageID, agentID)
	if err != nil {
		return nil, fmt.Errorf("store: list claims: %w", err)
	}
	defer rows.Close()
	var out []Claim
	for rows.Next() {
		var c Claim
		var ev string
		if err := rows.Scan(&c.ID, &c.PageID, &c.Text, &c.Status, &c.Confidence, &ev,
			&c.Freshness.Level, &c.Freshness.UpdatedAt, &c.Freshness.TTLDays); err != nil {
			return nil, err
		}
		unmarshalJSON(ev, &c.Evidence)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListClaimsForPageIDs fetches claims for many pages in one query (no
// per-page round trips). Result order is unspecified; callers group by
// Claim.PageID.
func (s *SQLiteStore) ListClaimsForPageIDs(ctx context.Context, agentID string, pageIDs []string) ([]Claim, error) {
	if len(pageIDs) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(pageIDs)+1)
	args = append(args, agentID)
	for _, id := range pageIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, page_id, text, status, confidence, evidence, freshness_level, updated_at, ttl_days
		FROM claims WHERE agent_id = ? AND page_id IN (`+placeholders(len(pageIDs))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list claims for pages: %w", err)
	}
	defer rows.Close()
	var out []Claim
	for rows.Next() {
		var c Claim
		var ev string
		if err := rows.Scan(&c.ID, &c.PageID, &c.Text, &c.Status, &c.Confidence, &ev,
			&c.Freshness.Level, &c.Freshness.UpdatedAt, &c.Freshness.TTLDays); err != nil {
			return nil, err
		}
		unmarshalJSON(ev, &c.Evidence)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ── contradictions ─────────────────────────────────────────────────────────

func (s *SQLiteStore) UpsertContradiction(ctx context.Context, c Contradiction) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO contradictions (id, agent_id, left_id, right_id, description, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, agent_id) DO UPDATE SET
			left_id = excluded.left_id,
			right_id = excluded.right_id,
			description = excluded.description,
			status = excluded.status`,
		c.ID, c.AgentID, c.LeftID, c.RightID, c.Description, c.Status, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: upsert contradiction: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListContradictions(ctx context.Context, agentID string) ([]Contradiction, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent_id, left_id, right_id, description, status, created_at
		FROM contradictions WHERE agent_id = ?`, agentID)
	if err != nil {
		return nil, fmt.Errorf("store: list contradictions: %w", err)
	}
	defer rows.Close()
	var out []Contradiction
	for rows.Next() {
		var c Contradiction
		if err := rows.Scan(&c.ID, &c.AgentID, &c.LeftID, &c.RightID, &c.Description, &c.Status, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountInterestPoints returns the number of interest points for an agent
// without materializing rows (used by the stats endpoint).
func (s *SQLiteStore) CountInterestPoints(ctx context.Context, agentID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM interest_points WHERE agent_id = ?`, agentID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count interest points: %w", err)
	}
	return n, nil
}

// CountPages returns the number of wiki pages for an agent.
func (s *SQLiteStore) CountPages(ctx context.Context, agentID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM wiki_pages WHERE agent_id = ?`, agentID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count pages: %w", err)
	}
	return n, nil
}

// CountContradictions returns the number of contradictions for an agent.
func (s *SQLiteStore) CountContradictions(ctx context.Context, agentID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM contradictions WHERE agent_id = ?`, agentID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count contradictions: %w", err)
	}
	return n, nil
}

// ── transcripts ───────────────────────────────────────────────────────────

func (s *SQLiteStore) SaveTranscript(ctx context.Context, t Transcript) error {
	var sd any
	if t.SessionDate != nil {
		sd = *t.SessionDate
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session_transcripts (session_id, agent_id, turn_count, raw_turns, received_at, session_date, processed_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT(session_id, agent_id) DO UPDATE SET
			turn_count = excluded.turn_count,
			raw_turns = excluded.raw_turns,
			received_at = excluded.received_at,
			session_date = excluded.session_date`,
		t.SessionID, t.AgentID, t.TurnCount, t.RawTurns, t.ReceivedAt, sd)
	if err != nil {
		return fmt.Errorf("store: save transcript: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetTranscript(ctx context.Context, agentID, sessionID string) (*Transcript, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT session_id, agent_id, turn_count, raw_turns, received_at, session_date, processed_at
		FROM session_transcripts WHERE session_id = ? AND agent_id = ?`, sessionID, agentID)
	var t Transcript
	var processed, sessionDate sql.NullTime
	err := row.Scan(&t.SessionID, &t.AgentID, &t.TurnCount, &t.RawTurns, &t.ReceivedAt, &sessionDate, &processed)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get transcript: %w", err)
	}
	if processed.Valid {
		t.ProcessedAt = &processed.Time
	}
	if sessionDate.Valid {
		t.SessionDate = &sessionDate.Time
	}
	return &t, nil
}

func (s *SQLiteStore) ListUnprocessedTranscripts(ctx context.Context, agentID string) ([]Transcript, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, agent_id, turn_count, raw_turns, received_at, session_date, processed_at
		FROM session_transcripts
		WHERE agent_id = ? AND processed_at IS NULL
		ORDER BY received_at ASC`, agentID)
	if err != nil {
		return nil, fmt.Errorf("store: list unprocessed transcripts: %w", err)
	}
	defer rows.Close()
	var out []Transcript
	for rows.Next() {
		var t Transcript
		var processed, sessionDate sql.NullTime
		if err := rows.Scan(&t.SessionID, &t.AgentID, &t.TurnCount, &t.RawTurns, &t.ReceivedAt, &sessionDate, &processed); err != nil {
			return nil, err
		}
		if processed.Valid {
			t.ProcessedAt = &processed.Time
		}
		if sessionDate.Valid {
			t.SessionDate = &sessionDate.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) MarkTranscriptProcessed(ctx context.Context, agentID, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE session_transcripts SET processed_at = ? WHERE session_id = ? AND agent_id = ?`,
		time.Now(), sessionID, agentID)
	if err != nil {
		return fmt.Errorf("store: mark transcript processed: %w", err)
	}
	return nil
}

var _ Store = (*SQLiteStore)(nil)
