package store

import "context"

// Replacement is the resolved successor of an archived/superseded entity:
// the far end of the sequel chain (old point --sequel--> new point),
// preferring the successor's wiki page when one exists.
type Replacement struct {
	// InterestPointID is the successor interest point (empty when the
	// successor is only reachable as a page).
	InterestPointID string
	// Page is the successor's wiki page when it exists (preferred target
	// for recall/search substitution).
	Page *Page
}

// ResolveReplacement walks the sequel chain from a possibly archived entity
// (interest point id or wiki page id) and returns its live successor, or nil
// when no sequel edge leads onward. A page id resolves to its backing
// interest point via the reverse has_page edge before walking sequels; the
// result prefers the successor page (resolved forward via has_page) over the
// bare successor interest point. Returns (nil, nil) when the entity is not
// archived and has no successor.
func (s *SQLiteStore) ResolveReplacement(ctx context.Context, agentID, id string) (*Replacement, error) {
	// Normalize to an interest-point id: a page id maps back through the
	// has_page edge to its backing interest point (or is used as-is).
	ipID := id
	if p, err := s.GetPage(ctx, agentID, id); err != nil {
		return nil, err
	} else if p != nil {
		ins, err := s.Backlinks(ctx, agentID, id)
		if err != nil {
			return nil, err
		}
		ipID = ""
		for _, e := range ins {
			if e.Kind == EdgeHasPage {
				ipID = e.SourceID
				break
			}
		}
	}

	// Walk sequels from the (resolved) interest point, staying on archived
	// points until a live successor appears.
	cur := ipID
	for cur != "" {
		outs, err := s.Outlinks(ctx, agentID, cur)
		if err != nil {
			return nil, err
		}
		var next string
		for _, e := range outs {
			if e.Kind == EdgeSequel {
				next = e.TargetID
				break
			}
		}
		if next == "" {
			return nil, nil
		}
		nextPt, err := s.GetInterestPoint(ctx, agentID, next)
		if err != nil {
			return nil, err
		}
		if nextPt == nil {
			return nil, nil
		}
		// Archive-to-archive sequels are possible mid-chain; keep walking.
		if nextPt.Status == "archived" {
			cur = next
			continue
		}
		// Live successor: prefer its wiki page, else return the interest point.
		rep := &Replacement{InterestPointID: next}
		outs, err = s.Outlinks(ctx, agentID, next)
		if err != nil {
			return nil, err
		}
		for _, e := range outs {
			if e.Kind == EdgeHasPage {
				if pg, err := s.GetPage(ctx, agentID, e.TargetID); err != nil {
					return nil, err
				} else if pg != nil {
					rep.Page = pg
					break
				}
			}
		}
		return rep, nil
	}
	return nil, nil
}
