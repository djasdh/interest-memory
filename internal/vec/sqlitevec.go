package vec

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

// SQLiteVec implements VectorIndex over sqlite-vec vec0 virtual tables,
// sharing the caller's *sql.DB (database/sql abstraction).
//
// One vec0 table per kind (interest_point / wiki_page), each with a fixed
// dimension set at construction. Tables are created lazily on first use.
type SQLiteVec struct {
	db         *sql.DB
	dimensions int
	mu         sync.Mutex
	tables     map[string]bool // created kind tables
	available  bool
}

// Init registers the sqlite-vec auto-extension. It must be called BEFORE any
// database/sql connection is opened (sqlite3_auto_extension only affects
// connections created afterwards). Idempotent; safe to call once at startup.
func Init() {
	sqlite_vec.Auto()
}

// NewSQLiteVec opens the vec layer on the shared database connection.
// Call Init() before the store opens its connection for the extension to load.
func NewSQLiteVec(db *sql.DB, dimensions int) (*SQLiteVec, error) {
	sqlite_vec.Auto()
	v := &SQLiteVec{
		db:         db,
		dimensions: dimensions,
		tables:     make(map[string]bool),
	}
	// Probe vec availability once.
	var ver string
	err := db.QueryRow("SELECT vec_version()").Scan(&ver)
	v.available = err == nil
	return v, nil
}

// Available reports whether sqlite-vec is operational.
func (v *SQLiteVec) Available() bool { return v.available }

// tableName returns the vec0 virtual table for a kind.
func tableName(kind string) string { return "vec_" + kind }

// ensureTable creates the vec0 table for a kind if not yet present.
func (v *SQLiteVec) ensureTable(ctx context.Context, kind string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.tables[kind] {
		return nil
	}
	name := tableName(kind)
	// vec0 rowid is auto; we store id + agent_id + kind as metadata columns.
	// sqlite-vec vec0 supports fixed float[N] + integer primary key + metadata
	// columns of type "text" that can be filtered with `WHERE ... = ?`.
	// NOTE: vec0 in this binding only accepts `chunk_size` as a table option
	// (no distance_metric), so KNN uses L2. Vectors are L2-normalized at
	// write/query time so L2 distance ranks identically to cosine similarity.
	ddl := fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING vec0(
		embedding float[%d],
		id text primary key,
		agent_id text,
		kind text
	)`, name, v.dimensions)
	if _, err := v.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("vec: create %s: %w", name, err)
	}
	v.tables[kind] = true
	return nil
}

// Upsert implements VectorIndex.
func (v *SQLiteVec) Upsert(ctx context.Context, e Entry) error {
	if !v.available {
		return fmt.Errorf("vec: sqlite-vec unavailable")
	}
	kind := e.Kind
	if kind == "" {
		kind = "wiki_page"
	}
	if err := v.ensureTable(ctx, kind); err != nil {
		return err
	}
	if len(e.Vector) != v.dimensions {
		return fmt.Errorf("vec: dimension mismatch: got %d want %d", len(e.Vector), v.dimensions)
	}
	name := tableName(kind)
	// vec0 has no reliable REPLACE semantics: INSERT OR REPLACE on a vec0
	// virtual table surfaces as UNIQUE constraint failed when the id exists
	// (observed in prod), which breaks pipeline retries. Delete-then-insert
	// is idempotent and safe.
	if _, err := v.db.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE id = ?`, name), e.ID); err != nil {
		return fmt.Errorf("vec: upsert %s: delete stale: %w", name, err)
	}
	if _, err := v.db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (embedding, id, agent_id, kind) VALUES (?, ?, ?, ?)`,
		name),
		encodeFloat32s(e.Vector), e.ID, e.AgentID, kind); err != nil {
		return fmt.Errorf("vec: upsert %s: %w", name, err)
	}
	return nil
}

// Search implements VectorIndex (KNN via vec0 MATCH).
func (v *SQLiteVec) Search(ctx context.Context, agentID string, q []float32, topK int) ([]Hit, error) {
	if !v.available {
		return nil, nil
	}
	if topK <= 0 {
		topK = 8
	}
	qb := encodeFloat32s(q) // normalize once, reuse across both kinds
	var out []Hit
	for _, kind := range []string{"interest_point", "wiki_page"} {
		if err := v.ensureTable(ctx, kind); err != nil {
			continue
		}
		name := tableName(kind)
		rows, err := v.db.QueryContext(ctx, fmt.Sprintf(
			`SELECT id, agent_id, distance FROM %s
			 WHERE agent_id = ? AND embedding MATCH ? ORDER BY distance LIMIT ?`,
			name), agentID, qb, topK)
		if err != nil {
			continue // table empty or error — skip kind
		}
		for rows.Next() {
			var id, ag string
			var dist float64
			if err := rows.Scan(&id, &ag, &dist); err != nil {
				rows.Close()
				return out, err
			}
			// Vectors are L2-normalized on write/query, so cosine similarity
			// is exactly 1 - L2²/2. Map the raw L2 distance to cosine so
			// scores are interpretable and MinScore thresholds are sane.
			cos := math.Max(-1, 1-(dist*dist)/2)
			score := float32(math.Max(0, cos))
			out = append(out, Hit{ID: id, AgentID: ag, Kind: kind, Score: score})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return out, err
		}
	}
	// Merge across kinds and return global top-K by score.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

// SearchByKeywords is a no-op pass-through for SQLiteVec; the FTS fallback
// implementation handles keyword-only search. Kept to satisfy VectorIndex.
func (v *SQLiteVec) SearchByKeywords(ctx context.Context, agentID, query string, topK int) ([]Hit, error) {
	return nil, nil
}

// Delete implements VectorIndex.
func (v *SQLiteVec) Delete(ctx context.Context, agentID, id string) error {
	if !v.available {
		return nil
	}
	for _, kind := range []string{"interest_point", "wiki_page"} {
		name := tableName(kind)
		// Tolerate tables that were never created (kind never indexed).
		if _, err := v.db.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE id = ? AND agent_id = ?`, name), id, agentID); err != nil {
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			return err
		}
	}
	return nil
}

// Close implements VectorIndex.
func (v *SQLiteVec) Close() error { return nil }

// encodeFloat32s serializes a vector to sqlite-vec's little-endian blob
// format, after L2-normalizing it. Normalization makes the vec0 KNN L2
// distance rank identically to cosine similarity (L2² = 2 − 2·cos for unit
// vectors), which matters because this binding's vec0 only accepts
// `chunk_size` as a table option — there is no distance_metric=cosine.
func encodeFloat32s(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	norm := v
	if n := l2Norm(v); n > 0 {
		norm = make([]float32, len(v))
		for i, f := range v {
			norm[i] = f / n
		}
	}
	for i, f := range norm {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func l2Norm(v []float32) float32 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	return float32(math.Sqrt(sum))
}

var _ VectorIndex = (*SQLiteVec)(nil)
