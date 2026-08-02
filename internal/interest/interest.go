package interest

import (
	"context"
	"fmt"
	"strings"

	"interest-memory/internal/config"
	"interest-memory/internal/store"
	"interest-memory/internal/vec"
	"interest-memory/internal/verify"
)

// Embedder computes embeddings for candidate text (implemented by
// *llm.Embedder).
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// VectorIndex is the recall surface for historical interest points
// (implemented by vec.SQLiteVec / vec.Fallback).
type VectorIndex interface {
	Search(ctx context.Context, agentID string, q []float32, topK int) ([]vec.Hit, error)
	Upsert(ctx context.Context, e vec.Entry) error
	Delete(ctx context.Context, agentID, id string) error
}

// Store is the persistence surface interest cleaning needs
// (implemented by *store.SQLiteStore).
type Store interface {
	GetInterestPoint(ctx context.Context, agentID, id string) (*store.InterestPoint, error)
	UpsertInterestPoint(ctx context.Context, p store.InterestPoint) error
	AddEdgePair(ctx context.Context, agentID string, e store.Edge) error
}

// Cleaner deduplicates/merges/relates verified candidates against historical
// interest points (design §五 step 3): embedding recall → >0.85 merge,
// 0.5~0.85 relate edge, else create. Returns the touched interest points AND
// the ids of archived historical points (relation delete/supersede) so the
// reconcile stage can cascade to their wiki pages.
type Cleaner interface {
	Clean(ctx context.Context, agentID string, verified []verify.Verified) ([]store.InterestPoint, []string, error)
}

type cleaner struct {
	embedder Embedder
	vec      VectorIndex
	store    Store
	cfg      config.ForkConfig
	topK     int
}

// New builds a Cleaner.
func New(embedder Embedder, vi VectorIndex, st Store, cfg config.ForkConfig) Cleaner {
	if cfg.SimilarityMerge <= 0 {
		cfg.SimilarityMerge = 0.85
	}
	if cfg.SimilarityRelate <= 0 {
		cfg.SimilarityRelate = 0.50
	}
	if cfg.ImportanceBoost <= 0 {
		cfg.ImportanceBoost = 0.05
	}
	return &cleaner{embedder: embedder, vec: vi, store: st, cfg: cfg, topK: 20}
}

// Clean processes each candidate: embed, recall historical points by
// similarity, then merge / relate / create. Returns the persisted interest
// points touched this run and the archived historical point ids.
func (c *cleaner) Clean(ctx context.Context, agentID string, verified []verify.Verified) ([]store.InterestPoint, []string, error) {
	var out []store.InterestPoint
	var archived []string
	for _, v := range verified {
		pt, arch, err := c.process(ctx, agentID, v)
		if err != nil {
			return out, archived, fmt.Errorf("interest: clean: %w", err)
		}
		if arch != "" {
			archived = append(archived, arch)
		}
		if pt != nil {
			out = append(out, *pt)
		}
	}
	return out, archived, nil
}

func (c *cleaner) process(ctx context.Context, agentID string, v verify.Verified) (*store.InterestPoint, string, error) {
	text := v.Candidate.Topic
	if v.Candidate.Reason != "" {
		text += "\n" + v.Candidate.Reason
	}
	vecV, err := c.embedder.Embed(ctx, text)
	if err != nil {
		return nil, "", err
	}

	hits, err := c.vec.Search(ctx, agentID, vecV, c.topK)
	if err != nil {
		return nil, "", err
	}
	var best *vec.Hit
	var bestSim float32
	for i := range hits {
		if hits[i].Kind != "interest_point" {
			continue
		}
		if hits[i].Score > bestSim {
			s := hits[i]
			best = &s
			bestSim = hits[i].Score
		}
	}

	now := store.Freshness{Level: v.Freshness.Level, UpdatedAt: v.Freshness.UpdatedAt, TTLDays: v.Freshness.TTLDays}

	// The verify#1 relation verdict takes precedence over pure similarity
	// thresholds: it decides how to touch the most similar historical point.
	switch v.Relation {
	case verify.RelationDelete:
		if v.RelationToID != "" {
			if err := c.archive(ctx, agentID, v.RelationToID); err != nil {
				return nil, "", err
			}
		}
		return nil, v.RelationToID, nil
	case verify.RelationSupersede:
		if v.RelationToID != "" {
			if err := c.archive(ctx, agentID, v.RelationToID); err != nil {
				return nil, "", err
			}
		}
		created, err := c.create(ctx, agentID, v, vecV, now)
		if err != nil {
			return nil, "", err
		}
		if v.RelationToID != "" {
			if err := c.store.AddEdgePair(ctx, agentID, store.Edge{
				SourceID: v.RelationToID, TargetID: created.ID,
				Kind: store.EdgeSequel, Weight: 1,
			}); err != nil {
				return nil, "", err
			}
		}
		return created, v.RelationToID, nil
	case verify.RelationUpdate:
		if v.RelationToID != "" {
			existing, err := c.store.GetInterestPoint(ctx, agentID, v.RelationToID)
			if err != nil {
				return nil, "", err
			}
			if existing != nil {
				merged := c.merge(existing, v)
				if err := c.store.UpsertInterestPoint(ctx, merged); err != nil {
					return nil, "", err
				}
				if err := c.vec.Upsert(ctx, c.entryFor(merged, vecV)); err != nil {
					return nil, "", err
				}
				return &merged, "", nil
			}
		}
		// Historical point vanished: fall through to the default path.
	}

	switch {
	case best != nil && float64(bestSim) >= c.cfg.SimilarityMerge:
		// Merge into the existing interest point.
		existing, err := c.store.GetInterestPoint(ctx, agentID, best.ID)
		if err != nil || existing == nil {
			// Stale vector: create fresh instead.
			pt, err := c.create(ctx, agentID, v, vecV, now)
			return pt, "", err
		}
		merged := c.merge(existing, v)
		if err := c.store.UpsertInterestPoint(ctx, merged); err != nil {
			return nil, "", err
		}
		if err := c.vec.Upsert(ctx, c.entryFor(merged, vecV)); err != nil {
			return nil, "", err
		}
		return &merged, "", nil

	case best != nil && float64(bestSim) >= c.cfg.SimilarityRelate:
		// Relate the new candidate to the historical one, then create.
		created, err := c.create(ctx, agentID, v, vecV, now)
		if err != nil {
			return nil, "", err
		}
		edge := store.Edge{SourceID: created.ID, TargetID: best.ID, Kind: store.EdgeRelated, Weight: float64(bestSim)}
		if err := c.store.AddEdgePair(ctx, agentID, edge); err != nil {
			return nil, "", err
		}
		return created, "", nil

	default:
		pt, err := c.create(ctx, agentID, v, vecV, now)
		return pt, "", err
	}
}

// archive marks an interest point archived and removes its vector so recall
// no longer surfaces it (the store record is kept for provenance).
func (c *cleaner) archive(ctx context.Context, agentID, id string) error {
	p, err := c.store.GetInterestPoint(ctx, agentID, id)
	if err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	if p.Status != "archived" {
		p.Status = "archived"
		if err := c.store.UpsertInterestPoint(ctx, *p); err != nil {
			return err
		}
	}
	return c.vec.Delete(ctx, agentID, id)
}

// create persists a new interest point and its vector.
func (c *cleaner) create(ctx context.Context, agentID string, v verify.Verified, vecV []float32, fresh store.Freshness) (*store.InterestPoint, error) {
	pt := store.InterestPoint{
		ID:             newID(v.Candidate.Topic),
		AgentID:        agentID,
		Name:           v.Candidate.Topic,
		Summary:        v.Candidate.Reason,
		Keywords:       v.Candidate.Tags,
		Importance:     v.Candidate.Confidence,
		Status:         "active",
		Subjective:     v.Subjective,
		TurnRange:      v.Candidate.TurnRange,
		Reliability:    v.Reliability,
		Freshness:      fresh,
		FirstSeenAt:    fresh.UpdatedAt,
		LastSeenAt:     fresh.UpdatedAt,
		SeenCount:      1,
		SourceSessions: []string{},
	}
	if err := c.store.UpsertInterestPoint(ctx, pt); err != nil {
		return nil, err
	}
	if err := c.vec.Upsert(ctx, c.entryFor(pt, vecV)); err != nil {
		return nil, err
	}
	return &pt, nil
}

// merge folds a verified candidate into an existing interest point.
func (c *cleaner) merge(existing *store.InterestPoint, v verify.Verified) store.InterestPoint {
	p := *existing
	p.SeenCount++
	p.LastSeenAt = v.Freshness.UpdatedAt
	p.Importance += c.cfg.ImportanceBoost + v.Candidate.Confidence*0.1
	// Prefer the freshest reliability/freshness, fold in new keywords.
	if v.Reliability.Status != "unknown" && v.Reliability.Confidence > p.Reliability.Confidence {
		p.Reliability = v.Reliability
	}
	p.Freshness = v.Freshness
	for _, kw := range v.Candidate.Tags {
		if !containsString(p.Keywords, kw) {
			p.Keywords = append(p.Keywords, kw)
		}
	}
	return p
}

func (c *cleaner) entryFor(p store.InterestPoint, vecV []float32) vec.Entry {
	return vec.Entry{
		ID:      p.ID,
		AgentID: p.AgentID,
		Kind:    "interest_point",
		Vector:  vecV,
		Metadata: map[string]string{
			"title": p.Name,
			"body":  p.Summary + " " + strings.Join(p.Keywords, " "),
		},
	}
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
