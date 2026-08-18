package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/vec"
)

// Graded is a recall hit annotated for injection (verify#3).
type Graded struct {
	Hit        vec.Hit
	Title      string
	Confidence float64
	Status     string    // supported | contested | unknown | stale
	FreshLevel string    // fresh | aging | stale | unknown
	EventTime  time.Time // event time (temporal injection)
	Note       string    // self-check hint
}

// Config controls the grading service.
type Config struct {
	// Language is the output language for grading prompts (unused by current
	// grading, kept for interface stability).
	Language string
}

// PointStore is the store surface grading needs (implemented by
// *store.SQLiteStore): read interest points/pages and resolve replacements.
type PointStore interface {
	GetInterestPoint(ctx context.Context, agentID, id string) (*store.InterestPoint, error)
	GetPage(ctx context.Context, agentID, id string) (*store.Page, error)
	// ListClaims fetches a page's claims (GetPage no longer loads them).
	ListClaims(ctx context.Context, agentID, pageID string) ([]store.Claim, error)
	// ResolveReplacement returns the live successor of an archived/superseded
	// entity (or nil) so grading can silently substitute it.
	ResolveReplacement(ctx context.Context, agentID, id string) (*store.Replacement, error)
}

// Verifier is the recall-grading surface (verify#3): annotates recall hits.
type Verifier interface {
	GradeForRecall(ctx context.Context, agentID string, hits []vec.Hit) ([]Graded, error)
}

type service struct {
	store PointStore
}

// New builds the grading service.
func New(st PointStore) Verifier {
	return &service{store: st}
}

// newID derives a stable id from a string.
func newID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])[:16]
}

var _ Verifier = (*service)(nil)
