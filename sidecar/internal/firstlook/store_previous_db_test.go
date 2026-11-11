package firstlook

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Previous is the newest report of a database saved before a given one
// (firstlook-05: the summary of an unchanged first look is reused).
func TestStorePreviousReturnsTheReportBefore(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	db := uniqueSchema("prev_")
	other := db + "_other"
	cleanReports(t, ctx, s, db)
	cleanReports(t, ctx, s, other)
	at := time.Now().UTC().Truncate(time.Millisecond)
	first := sampleReport()
	first.Database, first.StartedAt, first.FinishedAt = db, at, at
	id1, err := s.Save(ctx, first)
	if err != nil {
		t.Fatalf("save first: %v", err)
	}
	if err := s.SetSummary(ctx, id1, "first summary", "model-a"); err != nil {
		t.Fatalf("summary: %v", err)
	}
	if _, found, err := s.Previous(ctx, db, id1); err != nil || found {
		t.Fatalf("previous of the first report: found=%v err=%v, want none", found, err)
	}
	foreign := first
	foreign.Database = other
	idOther, err := s.Save(ctx, foreign)
	if err != nil {
		t.Fatalf("save other: %v", err)
	}
	second := first
	second.Items = second.Items[:1]
	id2, err := s.Save(ctx, second)
	if err != nil {
		t.Fatalf("save second: %v", err)
	}
	got, found, err := s.Previous(ctx, db, id2)
	if err != nil || !found {
		t.Fatalf("previous of %d: found=%v err=%v", id2, found, err)
	}
	if got.ID != id1 || got.Summary != "first summary" || got.SummaryModel != "model-a" ||
		len(got.Items) != len(first.Items) || got.Items[0].Rule != first.Items[0].Rule {
		t.Fatalf("previous = %+v, want report %d (not %d of %s) decoded", got, id1,
			idOther, other)
	}
}

func TestStorePreviousNeedsAPool(t *testing.T) {
	_, _, err := NewStore(nil).Previous(context.Background(), "app", 3)
	if !errors.Is(err, ErrNoPool) {
		t.Fatalf("err = %v, want ErrNoPool", err)
	}
}
