package recommendation

import (
	"errors"
	"testing"
)

func TestListFiltersByDatabaseAndState(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	db := uniqueDB(t)
	p := proposal(t, ctx, pool, db, sqlA, inverseA)
	a := mustPropose(t, ctx, s, p).Recommendation
	q := p
	q.ForwardSQL = "CREATE INDEX CONCURRENTLY idx_q ON public.orders (q)"
	b := mustPropose(t, ctx, s, q).Recommendation
	setState(t, ctx, pool, b.ID, StateApproved)

	all, err := s.List(ctx, ListFilter{DatabaseName: db})
	if err != nil || len(all) != 2 || all[0].ID != b.ID || all[1].ID != a.ID {
		t.Fatalf("list = %+v, %v, want newest first [b a]", all, err)
	}
	approved, err := s.List(ctx, ListFilter{DatabaseName: db, State: StateApproved})
	if err != nil || len(approved) != 1 || approved[0].ID != b.ID {
		t.Fatalf("approved filter = %+v, %v", approved, err)
	}
	limited, _ := s.List(ctx, ListFilter{DatabaseName: db, Limit: 1})
	if len(limited) != 1 {
		t.Fatalf("limit 1 returned %d rows", len(limited))
	}
	if _, err := s.List(ctx, ListFilter{DatabaseName: db, State: "bogus"}); err == nil {
		t.Fatal("an unknown state filter was accepted")
	}
	none, err := s.List(ctx, ListFilter{DatabaseName: db + "_none"})
	if err != nil || len(none) != 0 {
		t.Fatalf("empty database: %+v, %v", none, err)
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewStore(pool)
	if _, err := s.Get(ctx, -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get -1: err=%v, want ErrNotFound", err)
	}
	if revs, err := s.Revisions(ctx, -1); err != nil || len(revs) != 0 {
		t.Fatalf("revisions of missing: %v, %v", revs, err)
	}
}
