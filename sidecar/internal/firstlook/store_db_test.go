package firstlook

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func cleanReports(t *testing.T, ctx context.Context, s *Store, database string) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, "DELETE FROM sage.first_look WHERE database_name = $1",
		database); err != nil {
		t.Fatalf("clean: %v", err)
	}
}

func TestStoreSaveAndLatest(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	db := uniqueSchema("store_")
	cleanReports(t, ctx, s, db)
	if _, found, err := s.Latest(ctx, db); err != nil || found {
		t.Fatalf("empty store: found=%v err=%v", found, err)
	}
	start := time.Now().UTC().Truncate(time.Millisecond)
	r := Report{Database: db, Provider: "self-managed", StartedAt: start,
		FinishedAt: start.Add(1200 * time.Millisecond), DurationMS: 1200, Relations: 42,
		StatementTimeoutMS: 5000, Items: sampleReport().Items,
		Checks:       []Check{{Rule: RuleDuplicateIndex, Status: CheckFinding}},
		Capabilities: []Capability{{Name: CapHypoPG, Status: CapabilityMissing}}}
	r.Items[0].Evidence = []Evidence{{Source: "pg_index", Ref: "pg_index.indkey", Detail: "x"}}
	id, err := s.Save(ctx, r)
	if err != nil || id <= 0 {
		t.Fatalf("save: id=%d err=%v", id, err)
	}
	got, found, err := s.Latest(ctx, db)
	if err != nil || !found {
		t.Fatalf("latest: found=%v err=%v", found, err)
	}
	if got.ID != id || got.Database != db || got.Relations != 42 || got.DurationMS != 1200 ||
		len(got.Items) != 2 || got.Items[0].Evidence[0].Ref != "pg_index.indkey" ||
		len(got.Checks) != 1 || got.Capabilities[0].Name != CapHypoPG ||
		!got.StartedAt.Equal(start) || got.StatementTimeoutMS != 5000 {
		t.Fatalf("latest = %+v", got)
	}
	if err := s.SetSummary(ctx, id, "Two findings.", "fake-model"); err != nil {
		t.Fatalf("set summary: %v", err)
	}
	got, _, _ = s.Latest(ctx, db)
	if got.Summary != "Two findings." || got.SummaryModel != "fake-model" {
		t.Fatalf("summary = %q / %q", got.Summary, got.SummaryModel)
	}
}

func TestStoreKeepsOnlyRecentReports(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	db := uniqueSchema("retain_")
	cleanReports(t, ctx, s, db)
	var last int64
	for i := 0; i < retainReports+3; i++ {
		id, err := s.Save(ctx, Report{Database: db, StartedAt: time.Now(),
			FinishedAt: time.Now(), Relations: i})
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		last = id
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sage.first_look WHERE "+
		"database_name = $1", db).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != retainReports {
		t.Fatalf("kept %d reports, want %d", n, retainReports)
	}
	got, _, _ := s.Latest(ctx, db)
	if got.ID != last || got.Relations != retainReports+2 {
		t.Fatalf("latest = id %d relations %d, want the newest", got.ID, got.Relations)
	}
}

func TestStoreConcurrentSavesAreIsolatedPerDatabase(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	names := make([]string, 8)
	for i := range names {
		names[i] = fmt.Sprintf("%s_%d", uniqueSchema("conc_"), i)
		cleanReports(t, ctx, s, names[i])
	}
	for i := range names {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Save(ctx, Report{Database: names[i], Relations: i,
				StartedAt: time.Now(), FinishedAt: time.Now()})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent save: %v", err)
		}
	}
	for i, name := range names {
		got, found, err := s.Latest(ctx, name)
		if err != nil || !found || got.Relations != i {
			t.Fatalf("%s: found=%v relations=%d err=%v", name, found, got.Relations, err)
		}
	}
}

func TestStoreErrors(t *testing.T) {
	var nilStore *Store
	if _, err := nilStore.Save(context.Background(), Report{Database: "x"}); !errors.Is(err,
		ErrNoPool) {
		t.Fatalf("nil store save err = %v", err)
	}
	if _, _, err := NewStore(nil).Latest(context.Background(), "x"); !errors.Is(err,
		ErrNoPool) {
		t.Fatalf("nil pool latest err = %v", err)
	}
	pool, ctx := livePool(t)
	if _, err := NewStore(pool).Save(ctx, Report{}); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("empty database save err = %v, want ErrNoDatabase", err)
	}
	if err := NewStore(pool).SetSummary(ctx, -1, "x", "m"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id summary err = %v, want ErrNotFound", err)
	}
}
