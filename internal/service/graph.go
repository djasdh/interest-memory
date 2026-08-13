package service

import (
	"context"
	"fmt"

	"github.com/djasdh/interest-memory/internal/store"
)

// GraphNode is one node in the visualization graph (interest point or wiki
// page) with the fields a renderer needs — no body/summary/claims (those
// would balloon the payload on 10k+ node graphs).
type GraphNode struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"` // interest_point | wiki_page
	Title      string   `json:"title"`
	Status     string   `json:"status"` // active | archived | superseded
	PageType   string   `json:"page_type,omitempty"`
	Importance float64  `json:"importance,omitempty"`
	Subjective bool     `json:"subjective,omitempty"`
	Tags       []string `json:"tags,omitempty"`
}

// GraphEdge is one directed edge in the visualization graph. Source/Target
// reference GraphNode.ID (same id namespace so the renderer can connect them).
type GraphEdge struct {
	Source string         `json:"source"`
	Target string         `json:"target"`
	Kind   store.EdgeType `json:"kind"`
	Weight float64        `json:"weight"`
}

// Graph is the full agent graph for visualization. Nodes carry both entity
// kinds; edges are all five kinds, unfiltered (the renderer filters).
type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// ListGraph assembles the full visualization graph for an agent: interest
// points + wiki pages (medium node fields) and all edges. Interest-point and
// wiki ids are different namespaces that can theoretically collide, so when
// an id appears in both, node ids and edge endpoints are prefixed with their
// kind (interest_point:/wiki_page:) to keep the graph unambiguous. Edges
// whose endpoint id matches no node are dropped (they'd dangle).
func (s *Service) ListGraph(ctx context.Context, agentID string) (*Graph, error) {
	ips, err := s.store.ListInterestPoints(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("service: list graph interest points: %w", err)
	}
	pages, err := s.store.ListPages(ctx, agentID, "")
	if err != nil {
		return nil, fmt.Errorf("service: list graph pages: %w", err)
	}
	edges, err := s.store.ListEdges(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("service: list graph edges: %w", err)
	}

	// Resolve each bare entity id to its (kind, nodeID). Detect collisions.
	kindOf := map[string]string{}  // bare id → kind (first write wins)
	prefixed := map[string]bool{}  // bare ids that appear in both namespaces
	for _, p := range ips {
		if _, dup := kindOf[p.ID]; dup {
			prefixed[p.ID] = true
		} else {
			kindOf[p.ID] = "interest_point"
		}
	}
	for _, pg := range pages {
		if k, dup := kindOf[pg.ID]; dup {
			if k != "wiki_page" {
				prefixed[pg.ID] = true
			}
		} else {
			kindOf[pg.ID] = "wiki_page"
		}
	}

	g := &Graph{Nodes: []GraphNode{}, Edges: []GraphEdge{}}
	seen := map[string]bool{}
	for _, p := range ips {
		nid := p.ID
		if prefixed[p.ID] {
			nid = "interest_point:" + p.ID
		}
		g.Nodes = append(g.Nodes, GraphNode{
			ID: nid, Kind: "interest_point", Title: p.Name, Status: p.Status,
			Importance: p.Importance, Subjective: p.Subjective, Tags: p.Keywords,
		})
		seen[nid] = true
	}
	for _, pg := range pages {
		nid := pg.ID
		if prefixed[pg.ID] {
			nid = "wiki_page:" + pg.ID
		}
		g.Nodes = append(g.Nodes, GraphNode{
			ID: nid, Kind: "wiki_page", Title: pg.Title, Status: pg.Status,
			PageType: string(pg.PageType), Tags: pg.Tags,
		})
		seen[nid] = true
	}

	// Endpoint resolution: prefixed ids need their namespace disambiguated via
	// kindOf; a collided endpoint (both kinds) is dropped as ambiguous.
	for _, e := range edges {
		src, ok := resolveNode(e.SourceID, kindOf, prefixed)
		if !ok {
			continue
		}
		dst, ok := resolveNode(e.TargetID, kindOf, prefixed)
		if !ok {
			continue
		}
		if !seen[src] || !seen[dst] {
			continue
		}
		g.Edges = append(g.Edges, GraphEdge{Source: src, Target: dst, Kind: e.Kind, Weight: e.Weight})
	}
	return g, nil
}

// resolveNode maps a bare edge endpoint to its graph node id. Prefixed ids
// get the kind prefix; an endpoint whose kind can't be determined (collided
// bare id in both namespaces, or unknown id) is dropped.
func resolveNode(bare string, kindOf map[string]string, prefixed map[string]bool) (string, bool) {
	kind, known := kindOf[bare]
	if !known {
		return "", false
	}
	if prefixed[bare] {
		return "", false
	}
	if kind != "interest_point" && kind != "wiki_page" {
		return "", false
	}
	return bare, true
}
