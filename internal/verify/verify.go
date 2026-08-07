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
	"interest-memory/internal/websearch"
)

// LLM is the chat surface the correction layer needs (implemented by
// *llm.Client). Kept narrow for test fakes.
type LLM interface {
	ChatJSON(ctx context.Context, messages []llm.Message, out any) error
}

// Embedder computes the embedding used to recall the most similar historical
// interest point (implemented by *llm.Embedder). EmbedBatch is used by
// FlagContradictions to semantically group claims before asking the LLM.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// Retriever recalls historical interest points by vector (implemented by
// vec.VectorIndex).
type Retriever interface {
	Search(ctx context.Context, agentID string, q []float32, topK int) ([]vec.Hit, error)
}

// PointStore is the store surface verify needs (implemented by
// *store.SQLiteStore): read interest points for grading and update them
// during feedback write-back.
type PointStore interface {
	GetInterestPoint(ctx context.Context, agentID, id string) (*store.InterestPoint, error)
	UpsertInterestPoint(ctx context.Context, p store.InterestPoint) error
	GetPage(ctx context.Context, agentID, id string) (*store.Page, error)
	// ResolveReplacement returns the live successor of an archived/superseded
	// entity (or nil) so grading can silently substitute it.
	ResolveReplacement(ctx context.Context, agentID, id string) (*store.Replacement, error)
}

// Relation is how a new candidate relates to its most similar historical
// interest point (decided by the LLM during verify#1).
type Relation string

const (
	RelationNone      Relation = "none"      // unrelated/new, leaves existing points untouched
	RelationSupersede Relation = "supersede" // supersede: the new candidate replaces the old point (old archived + new created)
	RelationUpdate    Relation = "update"    // update: the new candidate corrects/refines the old point (merged into it)
	RelationDelete    Relation = "delete"    // delete: the new candidate overturns the old point (old archived, nothing created)
)

// Verified is a fork candidate after fact-checking (verify#1): the LLM
// decided reliability (confidence/status/evidence), freshness, subjectivity
// and the relation to the most similar historical point.
type Verified struct {
	Candidate      fork.Candidate
	Subjective     bool
	Relation       Relation
	RelationToID   string
	RelationReason string
	Reliability    store.Reliability
	Freshness      store.Freshness
}

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

// Config controls the correction layer.
type Config struct {
	UseWebSearch   bool
	SearchMax      int
	WebSearchKey   string // optional
	WebTool        string
	LLM            config.LLMConfig
	MaxConcurrency int
	// Language is the output language for the correction-layer prompts
	// (defaults to "English" when empty). JSON field names stay English.
	Language string
	// MinConfidence is the floor for contradiction confidence (0~1); pairs
	// below it are dropped. <=0 disables the filter (default).
	MinConfidence float64
	// SimThreshold is the minimum embedding cosine similarity for a claim pair
	// to be considered same-topic candidates for the LLM. <=0 defaults to 0.45.
	SimThreshold float64
	// MaxCandidates bounds how many candidate pairs are submitted to the LLM
	// per window, keeping the top-most similar. <=0 defaults to 30.
	MaxCandidates int
}

// Verifier is the domain interface: three-stage correction + feedback loop.
type Verifier interface {
	// VerifyCandidates fact-checks fork candidates (verify#1): LLM fact-check
	// (+ web search); subjective candidates are exempt from the web search but
	// still LLM-checked, and the relation to the most similar historical point
	// (supersede/update/delete) is decided. Produces Reliability/Freshness/evidence.
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
	search websearch.Searcher
	vec    Retriever
	embed  Embedder
	cfg    Config
}

// New builds the correction service. Searcher may be nil — web fact-checking
// is then skipped (degraded to LLM-only evidence). vec/embed may be nil —
// historical recall (relation judgment) is then skipped (relation=none).
func New(llm LLM, st PointStore, search websearch.Searcher, ri Retriever, emb Embedder, cfg Config) Verifier {
	if cfg.SearchMax <= 0 {
		cfg.SearchMax = 5
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 4
	}
	if cfg.Language == "" {
		cfg.Language = "English"
	}
	if cfg.SimThreshold <= 0 {
		cfg.SimThreshold = 0.45
	}
	if cfg.MaxCandidates <= 0 {
		cfg.MaxCandidates = 30
	}
	return &service{llm: llm, store: st, search: search, vec: ri, embed: emb, cfg: cfg}
}

// newID derives a stable id from a string (used for claims/contradictions).
func newID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])[:16]
}

func now() time.Time { return time.Now().UTC() }

var _ Verifier = (*service)(nil)

// degradedVerifier returns the fallback verdict for a candidate whose
// fact-check failed — never blocks the pipeline.
func degradedVerifier(c fork.Candidate) Verified {
	return Verified{
		Candidate:  c,
		Subjective: c.Subjective,
		Relation:   RelationNone,
		Reliability: store.Reliability{
			Confidence: c.Confidence,
			Status:     "unknown",
			Evidence:   nil,
		},
		Freshness: store.Freshness{Level: "unknown", UpdatedAt: now(), TTLDays: 0},
	}
}
