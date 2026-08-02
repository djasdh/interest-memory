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
// interest point (implemented by *llm.Embedder).
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
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
}

// Relation is how a new candidate relates to its most similar historical
// interest point (decided by the LLM during verify#1).
type Relation string

const (
	RelationNone      Relation = "none"      // 无关/新增，不触碰旧点
	RelationSupersede Relation = "supersede" // 取代：新候选覆盖旧点（旧点归档 + 新点创建）
	RelationUpdate    Relation = "update"    // 更新：新候选修正/补充旧点内容（合并进旧点）
	RelationDelete    Relation = "delete"    // 删除：新候选推翻旧点（旧点归档，不创建新点）
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
	Status     string // supported | contested | unknown
	FreshLevel string // fresh | aging | stale | unknown
	Note       string // self-check hint
}

// Config controls the correction layer.
type Config struct {
	UseWebSearch   bool
	SearchMax      int
	WebSearchKey   string // optional
	WebTool        string
	LLM            config.LLMConfig
	MaxConcurrency int
}

// Verifier is the domain interface (design §六/§七): 三段式纠错 + 闭环。
type Verifier interface {
	// VerifyCandidates fact-checks fork candidates (verify#1): LLM 事实核查
	// (+ 网络搜索)，主观候选豁免联网但 LLM 核查照常，判定与最相似历史点的
	// 关系（supersede/update/delete），产出 Reliability/Freshness/evidence。
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
		Candidate: c,
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

