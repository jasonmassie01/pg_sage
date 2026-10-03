package runway

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Performance gate offender 5: one sequence_runway statement read the
// last value of up to a quarter of the lock table's worth of sequences
// (616-773 ms at 5,000 sequences, over the 500 ms incident budget), and a
// catalog larger than that was never read past the cap. The monitor now
// reads the catalog in slices of at most probes.SequenceScanCap sequences,
// one statement and one transaction each, so every sequence is read on
// every pass and no statement's work grows with the catalog.

func serverLockBudget(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT (current_setting('max_locks_per_transaction')::int8
		* (current_setting('max_connections')::int8
		   + current_setting('max_prepared_transactions')::int8) / 4)::int`).
		Scan(&n); err != nil {
		t.Fatalf("lock budget: %v", err)
	}
	return n
}

// createBigintSequences creates n never-used bigint sequences in schema
// sch (in batches, each its own transaction) and then zz_hot, a bigint
// sequence at 97.6% of its limit created last (the highest OID).
func createBigintSequences(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	sch string, n int) {
	t.Helper()
	q := pgx.Identifier{sch}.Sanitize()
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for from := 1; from <= n; from += 1000 {
			_, _ = pool.Exec(c, fmt.Sprintf(`DO $$ BEGIN FOR i IN %d..%d LOOP
				EXECUTE format('DROP SEQUENCE IF EXISTS %s.b_%%s', i); END LOOP; END $$`,
				from, min(from+999, n), q))
		}
		_, _ = pool.Exec(c, "DROP SCHEMA IF EXISTS "+q+" CASCADE")
		_, _ = pool.Exec(c, "DELETE FROM sage.runway_samples WHERE subject LIKE $1",
			sch+".%")
	})
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+q); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for from := 1; from <= n; from += 1000 {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DO $$ BEGIN FOR i IN %d..%d LOOP
			EXECUTE format('CREATE SEQUENCE %s.b_%%s AS bigint', i); END LOOP; END $$`,
			from, min(from+999, n), q)); err != nil {
			t.Fatalf("create sequences from %d: %v", from, err)
		}
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SEQUENCE %[1]s.zz_hot AS bigint;
		SELECT setval('%[1]s.zz_hot', 9000000000000000000)`, q)); err != nil {
		t.Fatalf("hot sequence: %v", err)
	}
}

func TestReadSequences_EveryPassReadsTheWholeCatalogInSlices(t *testing.T) {
	pool, ctx := livePool(t)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	n := serverLockBudget(t, ctx, pool) + 300
	if n > 8000 {
		t.Skipf("the server's lock budget (%d sequences) needs a fixture over 8,000 "+
			"sequences", n-300)
	}
	sch := fmt.Sprintf("runway_slices_%d", time.Now().UnixNano())
	createBigintSequences(t, ctx, pool, sch, n)
	logs := &logSink{}
	opts := testOptions()
	opts.Retention, opts.Interval, opts.SequenceInterval = 48*time.Hour, time.Minute, 0
	m, err := NewMonitor(pool, probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1)),
		nil, opts, logs.log)
	if err != nil {
		t.Fatalf("NewMonitor: %v", err)
	}
	var snap Snapshot
	if err := m.readSequences(ctx, &snap); err != nil {
		t.Fatalf("read sequences: %v", err)
	}
	var hot *probes.SequenceRunway
	for i := range snap.Sequences {
		if snap.Sequences[i].Sequence == sch+".zz_hot" {
			hot = &snap.Sequences[i]
		}
	}
	if !snap.SequencesOK || hot == nil {
		t.Fatalf("zz_hot (created last, past a single statement's cap) was not read: "+
			"ok=%v, %d listed", snap.SequencesOK, len(snap.Sequences))
	}
	if math.Abs(hot.Fraction-0.9758) > 0.001 {
		t.Fatalf("zz_hot fraction = %v, want ~0.976", hot.Fraction)
	}
	if len(logs.matching("scan cap reached")) != 0 {
		t.Fatalf("a pass left sequences unread: %v", logs.lines)
	}
	for i := 1; i < len(snap.Sequences); i++ {
		if snap.Sequences[i].Fraction > snap.Sequences[i-1].Fraction {
			t.Fatalf("merged list not nearest the limit first at %d", i)
		}
	}
}
