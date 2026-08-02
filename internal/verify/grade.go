package verify

import (
	"context"
	"fmt"
	"time"

	"interest-memory/internal/vec"
)

// GradeForRecall annotates recall hits (verify#3): resolves the entity's
// stored reliability/freshness and attaches a self-check hint for the
// consuming agent. Archived interest points are filtered out (not injected).
func (s *service) GradeForRecall(ctx context.Context, agentID string, hits []vec.Hit) ([]Graded, error) {
	out := make([]Graded, 0, len(hits))
	for _, h := range hits {
		g := Graded{
			Hit:        h,
			Confidence: 0,
			Status:     "unknown",
			FreshLevel: "unknown",
			Note:       "may be outdated or inaccurate — please verify on your own",
		}
		title, conf, status, fresh, evt, skip := s.loadEntity(ctx, agentID, h)
		if skip {
			continue
		}
		g.Title = title
		g.Confidence = conf
		g.Status = status
		g.FreshLevel = fresh
		g.EventTime = evt
		out = append(out, g)
	}
	return out, nil
}

// loadEntity resolves an interest point or wiki page hit into gradable data.
// skip=true means the entity should not be recalled (e.g. archived).
func (s *service) loadEntity(ctx context.Context, agentID string, h vec.Hit) (title string, conf float64, status, fresh string, evt time.Time, skip bool) {
	if h.Kind == "interest_point" {
		p, err := s.store.GetInterestPoint(ctx, agentID, h.ID)
		if err != nil || p == nil {
			return title, 0, "unknown", "unknown", time.Time{}, false
		}
		if p.Status == "archived" {
			return "", 0, "", "", time.Time{}, true
		}
		return p.Name, p.Reliability.Confidence, p.Reliability.Status, p.Freshness.Level, p.EventTime, false
	}
	pg, err := s.store.GetPage(ctx, agentID, h.ID)
	if err != nil || pg == nil {
		return title, 0, "unknown", "unknown", time.Time{}, false
	}
	// Non-active pages (superseded/archived) are not recalled.
	if pg.Status != "" && pg.Status != "active" {
		return "", 0, "", "", time.Time{}, true
	}
	// Page-level grading: derive from the strongest claim, if any.
	if len(pg.Claims) == 0 {
		return pg.Title, 0, "unknown", "unknown", pg.EventTime, false
	}
	best := pg.Claims[0]
	for _, c := range pg.Claims {
		if c.Confidence > best.Confidence {
			best = c
		}
	}
	return pg.Title, best.Confidence, best.Status, best.Freshness.Level, pg.EventTime, false
}

// FeedbackWrite closes the loop: for each recalled interest-point hit, bump
// seen_count / importance / freshness timestamps (再次写入闭环).
func (s *service) FeedbackWrite(ctx context.Context, agentID string, hits []vec.Hit) error {
	for _, h := range hits {
		if h.Kind != "interest_point" {
			continue
		}
		p, err := s.store.GetInterestPoint(ctx, agentID, h.ID)
		if err != nil || p == nil {
			continue
		}
		p.SeenCount++
		p.LastSeenAt = now()
		p.Importance += 0.05
		p.Freshness.UpdatedAt = now()
		if err := s.store.UpsertInterestPoint(ctx, *p); err != nil {
			return fmt.Errorf("verify: feedback write: %w", err)
		}
	}
	return nil
}
