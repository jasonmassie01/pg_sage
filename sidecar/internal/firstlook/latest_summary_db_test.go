package firstlook

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestLatestSummaryMatchesLatest: the summary read in SQL counts exactly
// what Latest's decoded report counts, for the newest report only.
func TestLatestSummaryMatchesLatest(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	db := uniqueSchema("lsum_")
	cleanReports(t, ctx, s, db)
	if _, found, err := s.LatestSummary(ctx, db); err != nil || found {
		t.Fatalf("empty store: found=%v err=%v", found, err)
	}
	start := time.Now().UTC().Truncate(time.Millisecond)
	older := Report{Database: db, StartedAt: start, FinishedAt: start,
		Items: sampleReport().Items[:1]}
	if _, err := s.Save(ctx, older); err != nil {
		t.Fatalf("save older: %v", err)
	}
	r := summarySample()
	r.Database, r.StartedAt, r.FinishedAt = db, start, start.Add(812*time.Millisecond)
	id, err := s.Save(ctx, r)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.SetSummary(ctx, id, "Two findings.", "fake-model"); err != nil {
		t.Fatalf("set summary: %v", err)
	}
	full, found, err := s.Latest(ctx, db)
	if err != nil || !found {
		t.Fatalf("latest: found=%v err=%v", found, err)
	}
	got, found, err := s.LatestSummary(ctx, db)
	if err != nil || !found {
		t.Fatalf("latest summary: found=%v err=%v", found, err)
	}
	if want := SummaryOf(full); !reflect.DeepEqual(got, want) {
		t.Fatalf("LatestSummary = %+v\nSummaryOf(Latest) = %+v", got, want)
	}
	if got.ID != id || got.Items != 2 || got.Checks != (CheckCounts{Total: 3,
		Degraded: 1, Truncated: 1}) || got.Summary != "Two findings." ||
		!got.FinishedAt.Equal(r.FinishedAt) || got.DurationMS != 812 {
		t.Fatalf("LatestSummary = %+v", got)
	}
}

// A row written with an explicit empty section, or with no checks or
// items at all, counts like the report it decodes to.
func TestLatestSummaryOfSparseRows(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	db := uniqueSchema("lsum_sparse_")
	cleanReports(t, ctx, s, db)
	insertRow(t, s, `INSERT INTO sage.first_look (database_name, started_at,
		finished_at) VALUES ($1, now(), now())`, db)
	got, found, err := s.LatestSummary(ctx, db)
	if err != nil || !found || got.Items != 0 || got.Checks != (CheckCounts{}) ||
		len(got.Capabilities) != 0 {
		t.Fatalf("default row: %+v found=%v err=%v", got, found, err)
	}
	insertRow(t, s, `INSERT INTO sage.first_look (database_name, started_at,
		finished_at, checks) VALUES ($1, now(), now(),
		'[{"rule":"r1","section":"","status":"degraded"},{"rule":"r2","status":"ok"}]')`, db)
	full, _, err := s.Latest(ctx, db)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	got, _, err = s.LatestSummary(ctx, db)
	if err != nil || got.Checks != (CheckCounts{Total: 2, Degraded: 1}) ||
		got.Checks != SummaryOf(full).Checks {
		t.Fatalf("explicit empty section: %+v err=%v", got, err)
	}
}

func insertRow(t *testing.T, s *Store, sql, database string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql, database); err != nil {
		t.Fatalf("insert first look row: %v", err)
	}
}
