package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// SQLiteStore implements Store backed by a single SQLite database file.
//
// Concurrency design (preserving my-agent-core's multi-goroutine mechanism):
//   - A sync.RWMutex guards the *sql.DB handle for open/close.
//   - All writes from a single agent are serialized through an agent-keyed
//     mutex map (per-agent write queue), so concurrent EndSession jobs for
//     the same agent cannot interleave partial writes.
type SQLiteStore struct {
	mu sync.RWMutex
	db *sql.DB
	// agentMu serializes writes per agent_id.
	agentMu map[string]*sync.Mutex
	writeMu sync.Mutex // guards agentMu map
}

// maxOpenConns is the connection-pool size for file-backed databases. WAL
// mode permits many concurrent readers plus a single writer; the SQLite
// engine itself serializes writes (with busy_timeout as the wait), so the
// pool needs a handful of connections for read concurrency rather than one.
const maxOpenConns = 4

// Open opens (creating if needed) the SQLite database at path and applies
// schema migrations.
//
// File-backed databases open in WAL journal mode so concurrent readers never
// block the writer (and vice versa). ":memory:" keeps a single connection:
// it is a private per-connection database, so a pool would yield unrelated
// empty databases.
func Open(path string) (*SQLiteStore, error) {
	dsn := path
	maxConns := 1
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		dsn = path + "?_journal_mode=WAL&_busy_timeout=5000"
		maxConns = maxOpenConns
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxConns)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	s := &SQLiteStore{db: db, agentMu: make(map[string]*sync.Mutex)}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// DB exposes the underlying *sql.DB so the vec layer can share the same
// database/sql connection (single-binary, single-file store).
func (s *SQLiteStore) DB() *sql.DB {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db
}

// Close implements Store.
func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}

// agentLock returns the per-agent write mutex.
func (s *SQLiteStore) agentLock(agentID string) *sync.Mutex {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	m, ok := s.agentMu[agentID]
	if !ok {
		m = &sync.Mutex{}
		s.agentMu[agentID] = m
	}
	return m
}

// migrate creates all tables if not present and records schema version.
func (s *SQLiteStore) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS interest_points (
			id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			name TEXT NOT NULL,
			summary TEXT NOT NULL DEFAULT '',
			keywords TEXT NOT NULL DEFAULT '[]',
			importance REAL NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'active',
			subjective INTEGER NOT NULL DEFAULT 0,
			turn_range_start INTEGER NOT NULL DEFAULT 0,
			turn_range_end INTEGER NOT NULL DEFAULT 0,
			event_time TIMESTAMP,
			confidence REAL NOT NULL DEFAULT 0,
			reliability_status TEXT NOT NULL DEFAULT 'unknown',
			evidence TEXT NOT NULL DEFAULT '[]',
			freshness_level TEXT NOT NULL DEFAULT 'unknown',
			updated_at TIMESTAMP NOT NULL,
			ttl_days INTEGER NOT NULL DEFAULT 0,
			first_seen_at TIMESTAMP NOT NULL,
			last_seen_at TIMESTAMP NOT NULL,
			seen_count INTEGER NOT NULL DEFAULT 0,
			source_session_ids TEXT NOT NULL DEFAULT '[]',
			wiki_worthy INTEGER,
			PRIMARY KEY (id, agent_id)
		)`,
		`CREATE TABLE IF NOT EXISTS wiki_pages (
			id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			page_type TEXT NOT NULL,
			title TEXT NOT NULL,
			body_md TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active',
			tags TEXT NOT NULL DEFAULT '[]',
			sources TEXT NOT NULL DEFAULT '[]',
			event_time TIMESTAMP,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY (id, agent_id)
		)`,
		`CREATE TABLE IF NOT EXISTS edges (
			source_id TEXT NOT NULL,
			target_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			weight REAL NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL,
			agent_id TEXT NOT NULL,
			PRIMARY KEY (agent_id, source_id, target_id, kind)
		)`,
		`CREATE TABLE IF NOT EXISTS pending_links (
			agent_id TEXT NOT NULL,
			source_id TEXT NOT NULL,
			target TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			PRIMARY KEY (agent_id, source_id, target)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_target ON edges(agent_id, target_id)`,
		`CREATE TABLE IF NOT EXISTS claims (
			id TEXT NOT NULL,
			page_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			text TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'supported',
			confidence REAL NOT NULL DEFAULT 0,
			evidence TEXT NOT NULL DEFAULT '[]',
			freshness_level TEXT NOT NULL DEFAULT 'unknown',
			updated_at TIMESTAMP NOT NULL,
			ttl_days INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (id, agent_id)
		)`,
		`CREATE TABLE IF NOT EXISTS contradictions (
			id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			left_id TEXT NOT NULL,
			right_id TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'open',
			created_at TIMESTAMP NOT NULL,
			PRIMARY KEY (id, agent_id)
		)`,
		`CREATE TABLE IF NOT EXISTS session_transcripts (
			session_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			turn_count INTEGER NOT NULL DEFAULT 0,
			raw_turns TEXT NOT NULL DEFAULT '',
			received_at TIMESTAMP NOT NULL,
			session_date TIMESTAMP,
			processed_at TIMESTAMP,
			PRIMARY KEY (session_id, agent_id)
		)`,
		`CREATE TABLE IF NOT EXISTS change_log (
			id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			entity_kind TEXT NOT NULL DEFAULT '',
			entity_id TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL DEFAULT '',
			edges TEXT NOT NULL DEFAULT '[]',
			PRIMARY KEY (id, agent_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_change_log_agent ON change_log(agent_id, created_at DESC)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	_, _ = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO schema_version (version) VALUES (1)`)
	// Best-effort migration for existing databases created before newer columns
	// existed (duplicate-column errors are ignored).
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE interest_points ADD COLUMN subjective INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE interest_points ADD COLUMN turn_range_start INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE interest_points ADD COLUMN turn_range_end INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE wiki_pages ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`)
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE wiki_pages ADD COLUMN tags TEXT NOT NULL DEFAULT '[]'`)
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE wiki_pages ADD COLUMN sources TEXT NOT NULL DEFAULT '[]'`)
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE session_transcripts ADD COLUMN session_date TIMESTAMP`)
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE interest_points ADD COLUMN event_time TIMESTAMP`)
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE wiki_pages ADD COLUMN event_time TIMESTAMP`)
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE interest_points ADD COLUMN wiki_worthy INTEGER`)
	return nil
}

// ListAgentIDs returns the distinct namespaces that have persisted interest
// points or wiki pages (empty entries are skipped). Used by the "all"
// namespace-sharing mode.
func (s *SQLiteStore) ListAgentIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id FROM interest_points UNION SELECT agent_id FROM wiki_pages`)
	if err != nil {
		return nil, fmt.Errorf("store: list agent ids: %w", err)
	}
	defer rows.Close()
	var out []string
	seen := map[string]bool{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

// ── helpers ────────────────────────────────────────────────────────────────

// placeholders returns a comma-separated list of "?" SQL placeholders.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalJSON(data string, out any) {
	_ = json.Unmarshal([]byte(data), out)
}

// boolInt converts a bool to SQLite INTEGER storage.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullableBool converts a *bool to SQLite storage: nil → NULL, true → 1,
// false → 0.
func nullableBool(v *bool) any {
	if v == nil {
		return nil
	}
	if *v {
		return 1
	}
	return 0
}

// nullableTime returns nil for zero times so SQLite stores NULL.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
