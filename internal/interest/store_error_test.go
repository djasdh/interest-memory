package interest

import (
	"context"
	"errors"
	"testing"

	"github.com/djasdh/interest-memory/internal/config"
	"github.com/djasdh/interest-memory/internal/store"
	"github.com/djasdh/interest-memory/internal/vec"
	"github.com/djasdh/interest-memory/internal/verify"
)

// errStore returns an error from GetInterestPoint, simulating a store outage
// during the similarity-merge path.
type errStore struct {
	*fakeStore
}

func (e *errStore) GetInterestPoint(context.Context, string, string) (*store.InterestPoint, error) {
	return nil, errors.New("simulated db error")
}

// TestSimilarityMergePropagatesStoreError checks that a real store failure
// during the similarity-merge lookup is propagated instead of being mistaken
// for a stale vector. The old code treated any error as "stale vector" and
// silently created a duplicate interest point, corrupting the memory graph
// during a transient DB outage.
func TestSimilarityMergePropagatesStoreError(t *testing.T) {
	st := newFakeStore()
	c := New(fakeEmbedder{}, &fakeVec{hits: []vec.Hit{{ID: "hist1", Kind: "interest_point", Score: 0.95}}}, &errStore{fakeStore: st}, config.ForkConfig{})
	_, _, err := c.Clean(context.Background(), "a", []verify.Verified{vv("新主题", 0.9)})
	if err == nil {
		t.Fatalf("BUG: store error swallowed; cleaner created a point instead of returning the error (upsert=%+v)", st.upsert)
	}
	if st.upsert != nil {
		t.Errorf("cleaner persisted a duplicate point despite the store error: %+v", st.upsert)
	}
}
