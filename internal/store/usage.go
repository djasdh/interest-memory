package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UsageRow is one day's aggregated token usage.
type UsageRow struct {
	Date      string    `json:"date"`
	Input     int64     `json:"input_tokens"`
	Output    int64     `json:"output_tokens"`
	CacheHit  int64     `json:"cache_hit_tokens"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AddUsage increments the usage row for a date (YYYY-MM-DD) by the given
// delta, atomically (single upsert with x = x + excluded.x).
func (s *SQLiteStore) AddUsage(ctx context.Context, date string, input, output, cacheHit int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage (date, input_tokens, output_tokens, cache_hit_tokens, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(date) DO UPDATE SET
			input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			cache_hit_tokens = cache_hit_tokens + excluded.cache_hit_tokens,
			updated_at = excluded.updated_at`,
		date, input, output, cacheHit, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("store: add usage: %w", err)
	}
	return nil
}

// GetUsage returns one day's usage row, or nil when the day is absent.
func (s *SQLiteStore) GetUsage(ctx context.Context, date string) (*UsageRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT date, input_tokens, output_tokens, cache_hit_tokens, updated_at
		FROM usage WHERE date = ?`, date)
	var u UsageRow
	err := row.Scan(&u.Date, &u.Input, &u.Output, &u.CacheHit, &u.UpdatedAt)
	if err == nil {
		return &u, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return nil, fmt.Errorf("store: get usage: %w", err)
}

// ListUsage returns usage rows from (inclusive) a date onward, oldest first.
// An empty since returns all rows.
func (s *SQLiteStore) ListUsage(ctx context.Context, since string) ([]UsageRow, error) {
	q := `SELECT date, input_tokens, output_tokens, cache_hit_tokens, updated_at FROM usage`
	var rows *sql.Rows
	var err error
	if since == "" {
		rows, err = s.db.QueryContext(ctx, q+` ORDER BY date ASC`)
	} else {
		rows, err = s.db.QueryContext(ctx, q+` WHERE date >= ? ORDER BY date ASC`, since)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list usage: %w", err)
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.Date, &u.Input, &u.Output, &u.CacheHit, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
