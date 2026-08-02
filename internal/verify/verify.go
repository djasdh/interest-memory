package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"interest-memory/internal/config"
	"interest-memory/internal/fork"
	"interest-memory/internal/llm"
	"interest-memory/internal/store"
	"interest-memory/internal/vec"
)

// LLM is the chat surface the correction layer needs (implemented by
// *llm.Client). Kept narrow for test fakes.
type LLM interface {
	ChatJSON(ctx context.Context, messages []llm.Message, out any) error
}

// Searcher performs a web fact-check lookup (implemented by
// webSearchSearcher). Returns nil items when the search is disabled or fails.
type Searcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]SearchItem, error)
}

// SearchItem is a web search hit used as evidence.
type SearchItem struct {
	Title   string
	URL     string
	Snippet string
	Source  string
}

// PointStore is the store surface verify needs (implemented by
// *store.SQLiteStore): read interest points for grading and update them
// during feedback write-back.
type PointStore interface {
	GetInterestPoint(ctx context.Context, agentID, id string) (*store.InterestPoint, error)
	UpsertInterestPoint(ctx context.Context, p store.InterestPoint) error
	GetPage(ctx context.Context, agentID, id string) (*store.Page, error)
}

// Verified is a fork candidate after fact-checking (verify#1): the LLM
// decided reliability (confidence/status/evidence) and freshness.
type Verified struct {
	Candidate   fork.Candidate
	Reliability store.Reliability
	Freshness   store.Freshness
}

// Graded is a recall hit annotated for injection (verify#3).
type Graded struct {
	Hit        vec.Hit
	Title      string
	Confidence float64
	Status     string // supported | contested | unknown
	FreshLevel string // fresh | aging | stale | unknown
	Note       string // self-check hint
}

// Config controls the correction layer.
type Config struct {
	UseWebSearch bool
	SearchMax    int
	WebSearchKey string // optional
	LLM          config.LLMConfig
}

// Verifier is the domain interface (design §六/§七): 三段式纠错 + 闭环。
type Verifier interface {
	// VerifyCandidates fact-checks fork candidates (verify#1): LLM 事实核查
	// (+ 网络搜索预留), producing Reliability/Freshness/evidence.
	VerifyCandidates(ctx context.Context, agentID string, cands []fork.Candidate) ([]Verified, error)
	// CheckClaims extracts structured claims from interest points (verify#2).
	CheckClaims(ctx context.Context, agentID string, pts []store.InterestPoint) ([]store.Claim, error)
	// FlagContradictions detects contradiction pairs among claims (verify#2).
	FlagContradictions(ctx context.Context, agentID string, claims []store.Claim) ([]store.Contradiction, error)
	// GradeForRecall annotates recall hits for injection (verify#3).
	GradeForRecall(ctx context.Context, agentID string, hits []vec.Hit) ([]Graded, error)
	// FeedbackWrite closes the loop: bump seen_count/importance/freshness.
	FeedbackWrite(ctx context.Context, agentID string, hits []vec.Hit) error
}

type service struct {
	llm    LLM
	store  PointStore
	search Searcher
	cfg    Config
}

// New builds the correction service. Searcher may be nil — web fact-checking
// is then skipped (degraded to LLM-only evidence).
func New(llm LLM, st PointStore, search Searcher, cfg Config) Verifier {
	if cfg.SearchMax <= 0 {
		cfg.SearchMax = 5
	}
	return &service{llm: llm, store: st, search: search, cfg: cfg}
}

// newID derives a stable id from a string (used for claims/contradictions).
func newID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])[:16]
}

func now() time.Time { return time.Now().UTC() }

var _ Verifier = (*service)(nil)
