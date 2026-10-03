package collector

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// measured.md M4 (safety): the collector's sequences query held one
// AccessShareLock per sequence until its transaction ended (12,012 on
// lifeos every minute, 23% of the shared lock table; with PG defaults it
// would overflow). Sequences are now read in oid pages, one transaction
// per page, each page capped by a quarter of the lock table.

const (
	lockSeqUsed   = 5000
	lockSeqUnused = 200
)

// createManySequences makes lockSeqUsed sequences with distinct use
// (MAXVALUE 100000, value 20*i: 0.02% steps, so the kept top rows have no
// ties) and lockSeqUnused never-called ones.
func createManySequences(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS seq_lock CASCADE")
	})
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA seq_lock;
		DO $$ BEGIN
			FOR i IN 1..%d LOOP
				EXECUTE format('CREATE SEQUENCE seq_lock.u%%s MAXVALUE 100000', i);
				PERFORM setval(format('seq_lock.u%%s', i), i * 20);
			END LOOP;
			FOR i IN 1..%d LOOP
				EXECUTE format('CREATE SEQUENCE seq_lock.n%%s', i);
			END LOOP; END $$`, lockSeqUsed, lockSeqUnused)); err != nil {
		t.Fatalf("sequence fixture: %v", err)
	}
}

func legacySequences(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []SequenceStats {
	t.Helper()
	rows, err := pool.Query(ctx, legacySequencesSQL, SequenceFloorPct, SequenceTopN,
		SequenceMaxRows)
	if err != nil {
		t.Fatalf("legacy sequences: %v", err)
	}
	defer rows.Close()
	var out []SequenceStats
	for rows.Next() {
		var s SequenceStats
		if err := rows.Scan(&s.SchemaName, &s.SequenceName, &s.DataType, &s.LastValue,
			&s.MinValue, &s.MaxValue, &s.IncrementBy, &s.Cycle, &s.PctUsed); err != nil {
			t.Fatalf("legacy sequences scan: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("legacy sequences: %v", err)
	}
	return out
}

// lockCounter records the most locks any one catalog transaction held.
type lockCounter struct {
	max, queries int
	err          error
}

func (l *lockCounter) hook(ctx context.Context, tx pgx.Tx, _ string, _ []any) {
	var n int
	if err := tx.QueryRow(ctx,
		"SELECT count(*) FROM pg_locks WHERE pid = pg_backend_pid()").Scan(&n); err != nil {
		l.err = err
		return
	}
	l.queries++
	l.max = max(l.max, n)
}

func TestCollectSequences_LockCountBoundedWith5000Sequences(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	createManySequences(t, ctx, pool)
	want := legacySequences(t, ctx, pool)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	locks := &lockCounter{}
	c.onCatalogQuery = locks.hook
	got, err := c.collectSequences(ctx)
	if err != nil || locks.err != nil {
		t.Fatalf("collectSequences: %v (lock probe: %v)", err, locks.err)
	}
	if limit := SequencePageSize + 64; locks.max > limit {
		t.Fatalf("a collector transaction held %d locks, want at most %d", locks.max, limit)
	}
	if locks.queries < (lockSeqUsed+lockSeqUnused)/SequencePageSize {
		t.Fatalf("%d catalog transactions for %d sequences: not paged", locks.queries,
			lockSeqUsed+lockSeqUnused)
	}
	if asJSON(t, got) != asJSON(t, want) {
		t.Fatalf("sequences differ from the legacy view:\n got  %d rows %s\n want %d rows %s",
			len(got), qualified(got[:min(5, len(got))]), len(want),
			qualified(want[:min(5, len(want))]))
	}
	cov := c.sequenceCoverage()
	if !cov.Complete || cov.Scanned < lockSeqUsed+lockSeqUnused || cov.Used < lockSeqUsed ||
		cov.Unreadable != 0 {
		t.Fatalf("coverage = %+v, want complete with >= %d scanned and >= %d used", cov,
			lockSeqUsed+lockSeqUnused, lockSeqUsed)
	}
}

// One page never takes more than a quarter of the shared lock table, even
// when asked for more.
func TestReadSequencePage_ClampedToLockBudget(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	createManySequences(t, ctx, pool)
	var budget, total int
	if err := pool.QueryRow(ctx, `SELECT
		current_setting('max_locks_per_transaction')::int8 *
		(current_setting('max_connections')::int8 +
		 current_setting('max_prepared_transactions')::int8) / 4,
		(SELECT count(*) FROM pg_sequence q JOIN pg_class c ON c.oid = q.seqrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage'))`).
		Scan(&budget, &total); err != nil {
		t.Fatalf("budget: %v", err)
	}
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	locks := &lockCounter{}
	c.onCatalogQuery = locks.hook
	rows, limit, err := c.readSequencePage(ctx, 0, 1_000_000)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if limit != budget || len(rows) != min(total, budget) {
		t.Fatalf("page limit %d with %d rows, want limit %d and %d rows (%d sequences)",
			limit, len(rows), budget, min(total, budget), total)
	}
	if locks.max > budget+64 {
		t.Fatalf("page held %d locks over a budget of %d", locks.max, budget)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].oid <= rows[i-1].oid {
			t.Fatalf("page not in oid order at %d", i)
		}
	}
	if _, _, err := c.readSequencePage(ctx, 0, 0); err == nil {
		t.Fatal("a zero page size must be refused")
	}
}

// Past the per-cycle scan cap the collector rotates through the catalog,
// serving the rest from its last reads; after one full rotation the list
// equals a full read, and dropped sequences disappear.
func TestCollectSequences_RotatesUnderScanCap(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	createManySequences(t, ctx, pool)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	c.seqPageSize, c.seqScanCap = 500, 1500
	if _, err := c.collectSequences(ctx); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if cov := c.sequenceCoverage(); cov.Complete || cov.Scanned > 1500 {
		t.Fatalf("cycle 1 coverage %+v, want a partial scan of at most 1500", cov)
	}
	rotate := func(label string) {
		var got []SequenceStats
		for i := 0; i < (lockSeqUsed+lockSeqUnused)/1500+3; i++ {
			var err error
			if got, err = c.collectSequences(ctx); err != nil {
				t.Fatalf("%s: %v", label, err)
			}
		}
		if want := legacySequences(t, ctx, pool); asJSON(t, got) != asJSON(t, want) {
			t.Fatalf("%s: rotated list (%d rows) differs from a full read (%d rows)", label,
				len(got), len(want))
		}
	}
	rotate("after one rotation")
	if _, err := pool.Exec(ctx, `DO $$ BEGIN FOR i IN 4901..5000 LOOP
		EXECUTE format('DROP SEQUENCE seq_lock.u%s', i); END LOOP; END $$`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	rotate("after dropping the 100 most used")
}

// A failed page leaves the category unavailable (error) and the next cycle
// recovers.
func TestCollectSequences_PageErrorPropagates(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.collectSequences(canceled); err == nil {
		t.Fatal("collectSequences with a canceled context returned no error")
	}
	if _, err := c.collectSequences(ctx); err != nil {
		t.Fatalf("recovery cycle: %v", err)
	}
	if !c.sequenceCoverage().Complete {
		t.Fatalf("recovery cycle coverage %+v, want complete", c.sequenceCoverage())
	}
}

// The snapshot carries the sequence coverage of its cycle.
func TestCollect_SnapshotCarriesSequenceCoverage(t *testing.T) {
	pool := testPool(t)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	cov := snap.SequenceCoverage
	if cov == nil || !cov.Complete || cov.Scanned < cov.Used {
		t.Fatalf("snapshot coverage = %+v, want a complete scan", cov)
	}
}

// The caches are shared collector state: overlapping cycles (a slow cycle
// and a test or API-triggered collect) must serialize, not corrupt them.
func TestCatalogCaches_ConcurrentCycles(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	wantSeq, err := c.collectSequences(ctx)
	if err != nil {
		t.Fatalf("sequences: %v", err)
	}
	wantIdx, err := c.collectIndexes(ctx)
	if err != nil {
		t.Fatalf("indexes: %v", err)
	}
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		go func() {
			s, err := c.collectSequences(ctx)
			if err == nil && len(s) != len(wantSeq) {
				err = fmt.Errorf("%d sequences, want %d", len(s), len(wantSeq))
			}
			errs <- err
		}()
		go func() {
			idx, err := c.collectIndexes(ctx)
			if err == nil && len(idx) != len(wantIdx) {
				err = fmt.Errorf("%d indexes, want %d", len(idx), len(wantIdx))
			}
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent cycle: %v", err)
		}
	}
	if !c.sequenceCoverage().Complete {
		t.Fatalf("coverage after concurrent cycles: %+v", c.sequenceCoverage())
	}
}
