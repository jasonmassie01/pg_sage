package facts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// facts-09: coverage answers which objects a subject matches today, by the
// verifier's catalog patterns, before a person confirms or declares it.

func coverageSchemas(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	schemas ...string) {
	t.Helper()
	drop := func() {
		for _, s := range schemas {
			if _, err := pool.Exec(context.Background(),
				"DROP SCHEMA IF EXISTS "+s+" CASCADE"); err != nil {
				t.Errorf("drop schema %s: %v", s, err)
			}
		}
	}
	drop()
	t.Cleanup(drop)
	for _, s := range schemas {
		execAll(t, ctx, pool, "CREATE SCHEMA "+s)
	}
}

// indexes creates n indexes <schema>.idx_thesis_NN on one table.
func indexes(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string, n int) {
	t.Helper()
	execAll(t, ctx, pool, "CREATE TABLE "+schema+".runs (a int)")
	for i := 0; i < n; i++ {
		execAll(t, ctx, pool, fmt.Sprintf("CREATE INDEX idx_thesis_%02d ON %s.runs (a)", i,
			schema))
	}
}

func TestCoverageListsMatchingSchemas(t *testing.T) {
	pool, ctx := livePool(t)
	coverageSchemas(t, ctx, pool, "test_cov_b", "test_cov_a", "cov_other", "testxcov_c")
	got, err := CoverageOf(ctx, pool, KindSchema, "test_cov_*")
	if err != nil {
		t.Fatal(err)
	}
	// '_' is literal: testxcov_c does not match test_cov_*.
	if strings.Join(got.Objects, ",") != "test_cov_a,test_cov_b" || got.Truncated {
		t.Fatalf("coverage = %+v", got)
	}
	exact, err := CoverageOf(ctx, pool, KindSchema, "TEST_COV_A")
	if err != nil || strings.Join(exact.Objects, ",") != "test_cov_a" {
		t.Fatalf("an exact subject folds like the fact does: %+v %v", exact, err)
	}
}

// Boundary: 50 matches are all listed; 60 list the first 50 and say so.
func TestCoverageCapsAtFiftyObjects(t *testing.T) {
	pool, ctx := livePool(t)
	coverageSchemas(t, ctx, pool, "cov_sixty", "cov_fifty")
	indexes(t, ctx, pool, "cov_sixty", 60)
	indexes(t, ctx, pool, "cov_fifty", CoverageLimit)
	execAll(t, ctx, pool, "CREATE INDEX idxXthesis_99 ON cov_fifty.runs (a)")
	got, err := CoverageOf(ctx, pool, KindIndex, "cov_sixty.idx_thesis_*")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Objects) != CoverageLimit || !got.Truncated ||
		got.Objects[0] != "cov_sixty.idx_thesis_00" ||
		got.Objects[CoverageLimit-1] != "cov_sixty.idx_thesis_49" {
		t.Fatalf("60 matches: %d objects, truncated=%v, %v", len(got.Objects), got.Truncated,
			got.Objects)
	}
	fifty, err := CoverageOf(ctx, pool, KindIndex, "cov_fifty.idx_thesis_*")
	if err != nil {
		t.Fatal(err)
	}
	if len(fifty.Objects) != CoverageLimit || fifty.Truncated {
		t.Fatalf("exactly 50 matches: %d objects, truncated=%v", len(fifty.Objects),
			fifty.Truncated)
	}
	tables, err := CoverageOf(ctx, pool, KindTable, "cov_*.runs")
	if err != nil || strings.Join(tables.Objects, ",") != "cov_fifty.runs,cov_sixty.runs" {
		t.Fatalf("an index is not a table: %+v %v", tables, err)
	}
}

func TestCoverageOfSlots(t *testing.T) {
	pool, ctx := livePool(t)
	slot := "cov_slot_" + fmt.Sprint(len(t.Name()))
	execAll(t, ctx, pool, "SELECT pg_create_physical_replication_slot('"+slot+"')")
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			"SELECT pg_drop_replication_slot($1)", slot); err != nil {
			t.Errorf("drop slot %s: %v", slot, err)
		}
	})
	got, err := CoverageOf(ctx, pool, KindSlot, "cov_slot_*")
	if err != nil || strings.Join(got.Objects, ",") != slot {
		t.Fatalf("slot coverage: %+v %v", got, err)
	}
}

// Nil/empty: no match is an empty list, not nil.
func TestCoverageOfNothingIsEmpty(t *testing.T) {
	pool, ctx := livePool(t)
	got, err := CoverageOf(ctx, pool, KindTable, "nowhere_cov.*")
	if err != nil {
		t.Fatal(err)
	}
	if got.Objects == nil || len(got.Objects) != 0 || got.Truncated {
		t.Fatalf("no match = %#v", got)
	}
}

// Invalid input is refused before any query, with the fact errors.
func TestCoverageRefusesInvalidSubjects(t *testing.T) {
	cases := []struct {
		kind    Kind
		subject string
		want    error
	}{
		{KindIndex, "no_schema", ErrInvalidSubject},
		{KindSchema, "", ErrInvalidSubject},
		{KindSchema, "a.b", ErrInvalidSubject},
		{KindSchema, "s*", ErrProtectedSubject},
		{KindTable, "pg_catalog.pg_class", ErrProtectedSubject},
		{Kind("view"), "public.v", ErrInvalidKind},
		{Kind(""), "public.v", ErrInvalidKind},
	}
	for _, c := range cases {
		// A nil pool proves nothing is queried for a refused subject.
		_, err := CoverageOf(context.Background(), nil, c.kind, c.subject)
		if !errors.Is(err, c.want) {
			t.Fatalf("%s %q: err = %v, want %v", c.kind, c.subject, err, c.want)
		}
	}
}

// Error propagation: a failed catalog read names what was attempted.
func TestCoverageReportsACatalogFailure(t *testing.T) {
	pool, _ := livePool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := CoverageOf(ctx, pool, KindSchema, "test_*")
	if err == nil || !strings.Contains(err.Error(), "coverage of schema test_*") ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

// The read is bounded: read-only, under a 2 s statement timeout and a lock
// timeout, set for its own transaction only.
func TestCoverageReadIsBounded(t *testing.T) {
	pool, ctx := livePool(t)
	var stmt, lock, readOnly string
	err := boundedRead(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_setting('statement_timeout'),
			current_setting('lock_timeout'), current_setting('transaction_read_only')`).
			Scan(&stmt, &lock, &readOnly)
	})
	if err != nil || stmt != "2s" || lock == "0" || readOnly != "on" {
		t.Fatalf("statement_timeout %q lock_timeout %q read_only %q (%v)", stmt, lock,
			readOnly, err)
	}
	var after string
	if err := pool.QueryRow(ctx, "SHOW statement_timeout").Scan(&after); err != nil ||
		after == "2s" {
		t.Fatalf("the timeout leaked to the pool's sessions: %q %v", after, err)
	}
	slow := boundedRead(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT pg_sleep(3)")
		return err
	})
	if slow == nil || !strings.Contains(slow.Error(), "statement timeout") {
		t.Fatalf("a 3 s read was not cancelled at 2 s: %v", slow)
	}
}
