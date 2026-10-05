package firstlook

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Update records a retried report on the row its first attempt saved.
func TestStoreUpdateRecordsTheRetry(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	db := uniqueSchema("update_")
	cleanReports(t, ctx, s, db)
	start := time.Now().UTC().Truncate(time.Millisecond)
	first := Report{Database: db, StartedAt: start, FinishedAt: start, Relations: 0,
		StatementTimeoutMS: 5000,
		Checks: []Check{{Rule: RuleDuplicateIndex, Status: CheckDegraded,
			Note: "statement timeout (5000 ms) reached"}}}
	id, err := s.Save(ctx, first)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.SetSummary(ctx, id, "kept", "m"); err != nil {
		t.Fatalf("set summary: %v", err)
	}
	retried := first
	retried.ID, retried.Relations = id, 12
	retried.Items = sampleReport().Items
	retried.Checks = []Check{{Rule: RuleDuplicateIndex, Status: CheckFinding, Retried: true,
		Note: "first attempt: statement timeout (5000 ms) reached"}}
	retried.Capabilities = []Capability{{Name: CapHypoPG, Status: CapabilityMissing}}
	if err := s.Update(ctx, retried); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, found, err := s.Latest(ctx, db)
	if err != nil || !found {
		t.Fatalf("latest: found=%v err=%v", found, err)
	}
	if got.ID != id || got.Relations != 12 || len(got.Items) != len(retried.Items) ||
		len(got.Checks) != 1 || got.Checks[0] != retried.Checks[0] ||
		len(got.Capabilities) != 1 || got.Capabilities[0].Name != CapHypoPG {
		t.Fatalf("after update = %+v", got)
	}
	if !got.StartedAt.Equal(start) || got.StatementTimeoutMS != 5000 || got.Summary != "kept" {
		t.Fatalf("update changed what it does not own: %+v", got)
	}
	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sage.first_look WHERE "+
		"database_name = $1", db).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows = %d err %v, want the one report updated in place", rows, err)
	}
}

func TestStoreUpdateErrors(t *testing.T) {
	if err := NewStore(nil).Update(context.Background(), Report{ID: 1,
		Database: "x"}); !errors.Is(err, ErrNoPool) {
		t.Fatalf("nil pool err = %v, want ErrNoPool", err)
	}
	pool, ctx := livePool(t)
	s := NewStore(pool)
	if err := s.Update(ctx, Report{ID: 1}); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("no database err = %v, want ErrNoDatabase", err)
	}
	if err := s.Update(ctx, Report{ID: -1, Database: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id err = %v, want ErrNotFound", err)
	}
	// Another database's report is never overwritten through a wrong name.
	db := uniqueSchema("update_other_")
	cleanReports(t, ctx, s, db)
	id, err := s.Save(ctx, Report{Database: db, StatementTimeoutMS: 5000})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.Update(ctx, Report{ID: id, Database: db + "_x",
		Relations: 99}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong database err = %v, want ErrNotFound", err)
	}
	if got, _, _ := s.Latest(ctx, db); got.Relations != 0 {
		t.Fatalf("relations = %d: another database's update leaked", got.Relations)
	}
}
