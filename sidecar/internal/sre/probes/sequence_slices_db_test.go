package probes

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Performance gate offender 5 (v1.8.3): one sequence_runway statement may
// read at most SequenceScanCap sequences (each read opens and locks the
// sequence until the statement's transaction ends), so no statement's
// work or lock footprint grows with the catalog. A larger catalog is read
// in slices (Args.Slices, Args.Slice: the sequences whose OID hashes to
// the slice), each its own statement; the slices together cover every
// sequence exactly once, and every slice reports its coverage even when
// none of its sequences was ever used.

func sliceFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) string {
	t.Helper()
	sch := fmt.Sprintf("sre_slices_%d", time.Now().UnixNano())
	q := pgx.Identifier{sch}.Sanitize()
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for from := 1; from <= n; from += 1000 {
			_, _ = pool.Exec(c, fmt.Sprintf(`DO $$ BEGIN FOR i IN %d..%d LOOP
				EXECUTE format('DROP SEQUENCE IF EXISTS %s.b_%%s', i); END LOOP; END $$`,
				from, min(from+999, n), q))
		}
		_, _ = pool.Exec(c, "DROP SCHEMA IF EXISTS "+q+" CASCADE")
	})
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+q); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for from := 1; from <= n; from += 1000 {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DO $$ BEGIN FOR i IN %d..%d LOOP
			EXECUTE format('CREATE SEQUENCE %s.b_%%s AS bigint', i); END LOOP; END $$`,
			from, min(from+999, n), q)); err != nil {
			t.Fatalf("sequences from %d: %v", from, err)
		}
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SEQUENCE %[1]s.zz_hot AS bigint;
		SELECT setval('%[1]s.zz_hot', 9000000000000000000)`, q)); err != nil {
		t.Fatalf("hot sequence: %v", err)
	}
	return sch
}

func catalogSequences(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_sequence q
		JOIN pg_class c ON c.oid = q.seqrelid
		WHERE q.seqincrement > 0 AND NOT pg_is_other_temp_schema(c.relnamespace)`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSequenceRunway_SlicesCoverTheCatalogOnce(t *testing.T) {
	pool, ctx := livePool(t)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	sch := sliceFixture(t, ctx, pool, SequenceScanCap+600)
	total := catalogSequences(t, ctx, pool)
	slices := int((total + SequenceScanCap/2 - 1) / (SequenceScanCap / 2))
	var sum SequenceCoverage
	hot := 0
	for k := 0; k < slices; k++ {
		res := catalogRun(t, ctx, pool, SequenceRunwayProbe, Args{Slices: slices, Slice: k})
		c, err := SequenceCoverageOf(res)
		if err != nil {
			t.Fatalf("slice %d coverage: %v", k, err)
		}
		if c.Total == 0 || c.ScanCapped() || c.Scanned > SequenceScanCap {
			t.Fatalf("slice %d of %d coverage = %+v, want a non-empty uncapped slice", k,
				slices, c)
		}
		sum.Total += c.Total
		sum.Scanned += c.Scanned
		ss, err := Sequences(res)
		if err != nil {
			t.Fatalf("slice %d sequences: %v", k, err)
		}
		for _, s := range ss {
			if s.Sequence == sch+".zz_hot" {
				hot++
			}
		}
	}
	if sum.Total != total || sum.Scanned != total {
		t.Fatalf("slices covered %+v, want every one of %d sequences once", sum, total)
	}
	if hot != 1 {
		t.Fatalf("zz_hot listed by %d slices, want exactly 1", hot)
	}
	whole, err := SequenceCoverageOf(catalogRun(t, ctx, pool, SequenceRunwayProbe, Args{}))
	if err != nil || whole.Total != total || whole.Scanned > SequenceScanCap ||
		!whole.ScanCapped() {
		t.Fatalf("unsliced run = %+v (%v): want the whole catalog counted and at most "+
			"%d read", whole, err, SequenceScanCap)
	}
}

// Each slice's statement holds at most SequenceScanCap sequence locks.
func TestSequenceRunway_SliceLockFootprint(t *testing.T) {
	pool, ctx := livePool(t)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	sliceFixture(t, ctx, pool, SequenceScanCap+600)
	for _, args := range [][]any{{51, 0, 0}, {51, 2, 0}, {51, 2, 1}} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Query(ctx, sequenceRunwaySQL, args...)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("probe sql %v: %v", args, err)
		}
		rows.Close()
		var held int64
		err = tx.QueryRow(ctx, `SELECT count(*) FROM pg_locks
			WHERE pid = pg_backend_pid() AND locktype = 'relation'`).Scan(&held)
		_ = tx.Rollback(ctx)
		if err != nil || rows.Err() != nil {
			t.Fatalf("locks: %v %v", err, rows.Err())
		}
		if held > SequenceScanCap+50 {
			t.Fatalf("slice %v holds %d relation locks, want at most %d", args, held,
				SequenceScanCap)
		}
	}
}

func TestArgs_SliceBounds(t *testing.T) {
	reg := Catalog()
	ok := []Args{{}, {Slices: 1}, {Slices: 3, Slice: 2}, {Slices: MaxSequenceSlices,
		Slice: MaxSequenceSlices - 1}}
	for _, a := range ok {
		if err := reg.CheckArgs(SequenceRunwayProbe, a); err != nil {
			t.Errorf("args %+v rejected: %v", a, err)
		}
	}
	bad := []Args{{Slices: -1}, {Slices: 3, Slice: 3}, {Slices: 2, Slice: -1},
		{Slice: 1}, {Slices: MaxSequenceSlices + 1}, {Window: time.Hour},
		{Slices: 2, PID: 7}}
	for _, a := range bad {
		if err := reg.CheckArgs(SequenceRunwayProbe, a); !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("args %+v: err = %v, want ErrInvalidArgs", a, err)
		}
	}
	if err := reg.CheckArgs(XIDRunwayProbe, Args{Slices: 2}); !errors.Is(err,
		ErrInvalidArgs) {
		t.Errorf("a slice on a probe without slices: err = %v", err)
	}
}

// A slice without a used sequence still reports its coverage, on a row
// that lists no sequence.
func TestSequenceCoverage_CoverageOnlyRow(t *testing.T) {
	res := okResult(SequenceRunwayProbe, nil, Row{"sequence": nil, "coverage_only": true,
		"sequences_total": int64(1800), "sequences_scanned": int64(1800),
		"sequences_used": int64(0), "sequences_unreadable": int64(2)})
	ss, err := Sequences(res)
	if err != nil || len(ss) != 0 {
		t.Fatalf("sequences = %+v (%v), want none", ss, err)
	}
	c, err := SequenceCoverageOf(res)
	want := SequenceCoverage{Total: 1800, Scanned: 1800, Unreadable: 2}
	if err != nil || c != want {
		t.Fatalf("coverage = %+v (%v), want %+v", c, err, want)
	}
}
