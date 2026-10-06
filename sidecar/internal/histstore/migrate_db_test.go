package histstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

const scoped = "WHERE database_id = 7"

func TestMigrateToMetaCopiesEverythingReadably(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	seedMonitored(t, p.Monitored, day(t), 12)
	p.Noise(t, day(t).Add(time.Hour), 101, 202)
	srcSnaps, srcSamples := decoded(t, p.Monitored, ""), samples(t, p.Monitored, "")
	noiseBefore := decoded(t, p.Meta, "WHERE database_id = 8")

	rep := migrate(t, histstore.NewMonitored(p.Monitored), p.MetaStore(t),
		histstore.MigrateOptions{BatchRows: 7})

	snaps, qs := table(t, rep, "snapshots"), table(t, rep, "query_store")
	if !snaps.Complete || !qs.Complete {
		t.Fatalf("a full run must complete: %+v", rep)
	}
	if snaps.Copied != int64(len(srcSnaps)) || qs.Copied != int64(len(srcSamples)) {
		t.Fatalf("copied %d snapshots / %d samples, source has %d / %d", snaps.Copied,
			qs.Copied, len(srcSnaps), len(srcSamples))
	}
	equal(t, "snapshots decoded in the store", srcSnaps, decoded(t, p.Meta, scoped))
	equal(t, "query_store samples in the store", srcSamples, samples(t, p.Meta, scoped))
	equal(t, "the other database's history", noiseBefore,
		decoded(t, p.Meta, "WHERE database_id = 8"))
	equal(t, "source left as it was", srcSnaps, decoded(t, p.Monitored, ""))
	var crossed, deltas int
	if err := p.Meta.QueryRow(context.Background(), `SELECT
		count(*) FILTER (WHERE b.id IS NULL OR b.database_id IS DISTINCT FROM 7),
		count(*) FROM sage.snapshots d LEFT JOIN sage.snapshots b ON b.id = d.base_id
		WHERE d.database_id = 7 AND d.base_id IS NOT NULL`).Scan(&crossed, &deltas); err != nil {
		t.Fatal(err)
	}
	if deltas == 0 {
		t.Fatal("the fixture wrote no delta rows: base remapping is untested")
	}
	if crossed != 0 {
		t.Fatalf("%d copied deltas point at a base outside their database", crossed)
	}
}

func TestMigrateIsResumableAndIdempotent(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	seedMonitored(t, p.Monitored, day(t), 10)
	src, dst := histstore.NewMonitored(p.Monitored), p.MetaStore(t)
	first := migrate(t, src, dst, histstore.MigrateOptions{BatchRows: 5, MaxBatches: 1})
	if s := table(t, first, "snapshots"); s.Complete || s.Copied != 5 {
		t.Fatalf("an interrupted run copies one batch and is not complete: %+v", s)
	}
	rest := migrate(t, src, dst, histstore.MigrateOptions{BatchRows: 5})
	if !table(t, rest, "snapshots").Complete {
		t.Fatalf("resumed run must complete: %+v", rest)
	}
	mon, meta := p.Count(t, "snapshots")
	if mon != meta {
		t.Fatalf("resumed copy has %d rows for a %d-row source (duplicates or gaps)",
			meta, mon)
	}
	again := migrate(t, src, dst, histstore.MigrateOptions{})
	for _, tr := range again.Tables {
		if tr.Copied != 0 || !tr.Complete {
			t.Fatalf("a re-run must copy nothing: %+v", tr)
		}
	}
	equal(t, "snapshots after resume", decoded(t, p.Monitored, ""),
		decoded(t, p.Meta, scoped))
}

func TestMigrateCopiesRowsWrittenAfterAnEarlierRun(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	seedMonitored(t, p.Monitored, day(t), 6)
	src, dst := histstore.NewMonitored(p.Monitored), p.MetaStore(t)
	migrate(t, src, dst, histstore.MigrateOptions{})
	// The sidecar kept running in monitored mode: more cycles arrive.
	seedMonitored(t, p.Monitored, day(t).Add(5*time.Hour), 4)
	rep := migrate(t, src, dst, histstore.MigrateOptions{})
	if c := table(t, rep, "snapshots").Copied; c == 0 {
		t.Fatal("rows written after the first run must be copied")
	}
	equal(t, "snapshots after catch-up", decoded(t, p.Monitored, ""),
		decoded(t, p.Meta, scoped))
	equal(t, "samples after catch-up", samples(t, p.Monitored, ""),
		samples(t, p.Meta, scoped))
}

func TestMigrateSkipsDeltasWhoseBaseIsGone(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	seedMonitored(t, p.Monitored, day(t), 4)
	if _, err := p.Monitored.Exec(context.Background(), `INSERT INTO sage.snapshots
		(collected_at, category, data, base_id) VALUES (now(), 'tables', '{"n":1}', 999999999)`,
	); err != nil {
		t.Fatal(err)
	}
	rep := migrate(t, histstore.NewMonitored(p.Monitored), p.MetaStore(t),
		histstore.MigrateOptions{})
	s := table(t, rep, "snapshots")
	if s.Skipped != 1 || !s.Complete {
		t.Fatalf("one unreadable delta must be skipped and reported: %+v", s)
	}
	mon, meta := p.Count(t, "snapshots")
	if meta != mon-1 {
		t.Fatalf("store has %d rows, want %d", meta, mon-1)
	}
}

func TestCleanupNeedsACompleteCopyAndRemovesOnlyTheSource(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	seedMonitored(t, p.Monitored, day(t), 8)
	p.Noise(t, day(t), 101)
	src, dst := histstore.NewMonitored(p.Monitored), p.MetaStore(t)
	ctx := context.Background()
	if _, err := histstore.Cleanup(ctx, src, dst); !errors.Is(err, histstore.ErrIncomplete) {
		t.Fatalf("cleanup before any copy: want ErrIncomplete, got %v", err)
	}
	migrate(t, src, dst, histstore.MigrateOptions{BatchRows: 3, MaxBatches: 1})
	if _, err := histstore.Cleanup(ctx, src, dst); !errors.Is(err, histstore.ErrIncomplete) {
		t.Fatalf("cleanup of a partial copy: want ErrIncomplete, got %v", err)
	}
	migrate(t, src, dst, histstore.MigrateOptions{})
	want := decoded(t, p.Meta, scoped)
	rep, err := histstore.Cleanup(ctx, src, dst)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if rep.Removed["snapshots"] == 0 || rep.Removed["query_store"] == 0 {
		t.Fatalf("cleanup must report what it removed: %+v", rep)
	}
	if mon, _ := p.Count(t, "snapshots"); mon != 0 {
		t.Fatalf("source still holds %d snapshots", mon)
	}
	if mon, _ := p.Count(t, "query_store"); mon != 0 {
		t.Fatalf("source still holds %d samples", mon)
	}
	equal(t, "store after cleanup", want, decoded(t, p.Meta, scoped))
	if n := len(decoded(t, p.Meta, "WHERE database_id = 8")); n != 3 {
		t.Fatalf("cleanup touched another database's history: %d rows left", n)
	}
}

func TestMigrateBackToMonitored(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	seedMonitored(t, p.Monitored, day(t), 8)
	p.Noise(t, day(t), 101)
	mon, meta := histstore.NewMonitored(p.Monitored), p.MetaStore(t)
	migrate(t, mon, meta, histstore.MigrateOptions{})
	if _, err := histstore.Cleanup(context.Background(), mon, meta); err != nil {
		t.Fatalf("cleanup to meta: %v", err)
	}
	want := decoded(t, p.Meta, scoped)
	rep := migrate(t, meta, mon, histstore.MigrateOptions{BatchRows: 4})
	if !table(t, rep, "snapshots").Complete {
		t.Fatalf("copy back must complete: %+v", rep)
	}
	equal(t, "snapshots copied back", want, decoded(t, p.Monitored, ""))
	if _, err := histstore.Cleanup(context.Background(), meta, mon); err != nil {
		t.Fatalf("cleanup back: %v", err)
	}
	if _, n := p.Count(t, "snapshots"); n != 0 {
		t.Fatalf("store still holds %d rows of the database after cleanup", n)
	}
	if n := len(decoded(t, p.Meta, "WHERE database_id = 8")); n != 3 {
		t.Fatalf("cleanup back removed another database's rows: %d left", n)
	}
}

func TestMigrateRefusesSamePlacement(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	mon := histstore.NewMonitored(p.Monitored)
	if _, err := histstore.Migrate(ctx, mon, mon, histstore.MigrateOptions{}); !errors.Is(
		err, histstore.ErrSamePlacement) {
		t.Fatalf("monitored to monitored: want ErrSamePlacement, got %v", err)
	}
	meta := p.MetaStore(t)
	if _, err := histstore.Migrate(ctx, meta, meta, histstore.MigrateOptions{}); !errors.Is(
		err, histstore.ErrSamePlacement) {
		t.Fatalf("meta to meta: want ErrSamePlacement, got %v", err)
	}
	if _, err := histstore.Migrate(ctx, histstore.Store{}, meta,
		histstore.MigrateOptions{}); !errors.Is(err, histstore.ErrNoDatabase) {
		t.Fatalf("no source: want ErrNoDatabase, got %v", err)
	}
	if _, err := histstore.Migrate(ctx, mon, meta,
		histstore.MigrateOptions{BatchRows: -1}); err == nil {
		t.Fatal("a negative batch size must be refused")
	}
}

func TestConcurrentMigrationsNeverDuplicate(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	seedMonitored(t, p.Monitored, day(t), 10)
	src, dst := histstore.NewMonitored(p.Monitored), p.MetaStore(t)
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = histstore.Migrate(context.Background(), src, dst,
				histstore.MigrateOptions{BatchRows: 3})
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, histstore.ErrMigrationBusy):
			t.Fatalf("concurrent run failed with an unexpected error: %v", err)
		}
	}
	if ok == 0 {
		t.Fatal("no concurrent run succeeded")
	}
	migrate(t, src, dst, histstore.MigrateOptions{}) // finish if the winner was early
	mon, meta := p.Count(t, "snapshots")
	if mon != meta {
		t.Fatalf("concurrent runs left %d store rows for %d source rows", meta, mon)
	}
}
