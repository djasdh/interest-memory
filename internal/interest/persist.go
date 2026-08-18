package interest

import (
	"context"
	"fmt"
	"strings"

	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/vec"
)

// PersistStore is the persistence surface V1.3 needs (implemented by
// *store.SQLiteStore). Kept narrow for test fakes.
type PersistStore interface {
	UpsertInterestPoint(ctx context.Context, p store.InterestPoint) error
	AddEdgePairs(ctx context.Context, agentID string, edges []store.Edge) error
	AppendLog(ctx context.Context, l store.ChangeLog) error
	UpsertContradiction(ctx context.Context, c store.Contradiction) error
}

// PersistVec is the vector surface V1.3 needs (implemented by
// vec.VectorIndex).
type PersistVec interface {
	Upsert(ctx context.Context, e vec.Entry) error
	Delete(ctx context.Context, agentID, id string) error
}

// Persist writes an Adjudication (V1.2 output) to the store:
//   - final points (create/update) are upserted with their embeddings + logs
//   - archived historical points are marked archived, their vectors deleted
//   - contradictions are stored with bidirectional contradicts edges
//   - programmatic related edges are generated among all surviving points
//     whose pairwise embedding cosine ≥ relateSim, weight = cosine.
//
// This is the V1.3 stage: the whole batch must be adjudicated (V1.2) before
// anything is persisted here.
func Persist(ctx context.Context, agentID string, st PersistStore, vi PersistVec, adj Adjudication, relateSim float64) error {
	if relateSim <= 0 {
		relateSim = 0.50
	}

	// 1. Final points: create/update + vector + log.
	surviving := make([]FinalPoint, 0, len(adj.FinalPoints))
	for _, fp := range adj.FinalPoints {
		pt := fp.Point
		if err := st.UpsertInterestPoint(ctx, pt); err != nil {
			return fmt.Errorf("interest: persist point %s: %w", pt.ID, err)
		}
		if fp.Vec != nil {
			if err := vi.Upsert(ctx, persistEntry(pt, fp.Vec)); err != nil {
				return fmt.Errorf("interest: persist vector %s: %w", pt.ID, err)
			}
		}
		_ = st.AppendLog(ctx, store.ChangeLog{
			AgentID: agentID, EntityKind: "interest_point", EntityID: pt.ID,
			Title: pt.Name, Action: fp.Action,
		})
		surviving = append(surviving, fp)
	}

	// 2. Archived historical points: mark archived + delete vector + log.
	for _, ap := range adj.Archived {
		pt := ap.Pt
		pt.Status = "archived"
		if err := st.UpsertInterestPoint(ctx, pt); err != nil {
			return fmt.Errorf("interest: persist archive %s: %w", pt.ID, err)
		}
		if err := vi.Delete(ctx, agentID, pt.ID); err != nil {
			return fmt.Errorf("interest: persist archive vector %s: %w", pt.ID, err)
		}
		_ = st.AppendLog(ctx, store.ChangeLog{
			AgentID: agentID, EntityKind: "interest_point", EntityID: pt.ID,
			Title: pt.Name, Action: "archive",
		})
	}

	// 3. Contradictions: store + bidirectional contradicts edge.
	var edges []store.Edge
	for _, c := range adj.Contradictions {
		if err := st.UpsertContradiction(ctx, c); err != nil {
			return fmt.Errorf("interest: persist contradiction: %w", err)
		}
		edges = append(edges, store.Edge{
			SourceID: c.LeftID, TargetID: c.RightID, Kind: store.EdgeContradict, Weight: 1,
		})
	}

	// 4. Programmatic related edges among surviving points (cos ≥ relateSim).
	for i := range surviving {
		for j := i + 1; j < len(surviving); j++ {
			if surviving[i].Vec == nil || surviving[j].Vec == nil {
				continue
			}
			sim := cosine(surviving[i].Vec, surviving[j].Vec)
			if sim >= relateSim {
				edges = append(edges, store.Edge{
					SourceID: surviving[i].Point.ID, TargetID: surviving[j].Point.ID,
					Kind: store.EdgeRelated, Weight: sim,
				})
			}
		}
	}
	if len(edges) > 0 {
		if err := st.AddEdgePairs(ctx, agentID, edges); err != nil {
			return fmt.Errorf("interest: persist edges: %w", err)
		}
	}
	return nil
}

// persistEntry builds the vector entry for a persisted interest point.
func persistEntry(p store.InterestPoint, vecV []float32) vec.Entry {
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
