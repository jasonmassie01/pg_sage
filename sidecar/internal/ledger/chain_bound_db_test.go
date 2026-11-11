package ledger

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// audit-11 bound: the decision chain must not grow by a link per parked
// fingerprint per cycle (dogfood lifeos parks ~15,000 candidates). 15,000
// parked fingerprints repeated for 10 cycles add one insert link each,
// plus one state link per real change (here, 100 reasons in cycle 5).
// No concurrent access tests: the chain's writer ordering is covered by
// auditchain's concurrency tests; this pins the volume.
func TestParkedRepeatsAddAtMostOneLinkPerChange(t *testing.T) {
	pool, _ := requireLedgerPostgres(t)
	if testing.Short() {
		t.Skip("150,000 upserts: skipped in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	const fingerprints, cycles, changed = 15000, 10, 100
	db := ledgerDatabaseID()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.decision "+
			"WHERE database_id = $1", db)
	})
	start := time.Now()
	for c := 1; c <= cycles; c++ {
		runParkedCycle(ctx, t, pool, db, fingerprints, c, changed)
	}
	t.Logf("%d cycles of %d parked fingerprints in %s", cycles, fingerprints,
		time.Since(start))
	var rows, repeats int
	if err := pool.QueryRow(ctx, `SELECT count(*), min(repeat_count) FROM sage.decision
		WHERE database_id = $1`, db).Scan(&rows, &repeats); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	var inserts, updates, other int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE l.op = 'I'),
		count(*) FILTER (WHERE l.op = 'U'), count(*) FILTER (WHERE l.op NOT IN ('I', 'U'))
		FROM sage.audit_chain_link l JOIN sage.decision d ON d.id = l.row_id
		WHERE l.chain = 'decision' AND d.database_id = $1`, db).Scan(&inserts, &updates,
		&other); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if rows != fingerprints || repeats != cycles || inserts != fingerprints ||
		updates != changed || other != 0 {
		t.Fatalf("rows %d (repeats %d), links I=%d U=%d other=%d; want %d rows seen %d "+
			"times, %d insert links and %d state links", rows, repeats, inserts, updates,
			other, fingerprints, cycles, fingerprints, changed)
	}
}

// runParkedCycle records every fingerprint once, through the repository's
// own upsert statement, in one pipelined batch. In cycle 5 the first
// changed fingerprints get a new reason.
func runParkedCycle(ctx context.Context, t *testing.T, pool *pgxpool.Pool, db, n, cycle,
	changed int) {
	t.Helper()
	batch := &pgx.Batch{}
	for i := 0; i < n; i++ {
		in := parkedInput(fmt.Sprintf("bound%d_%d", db, i), db)
		in.EvidenceID = fmt.Sprintf("ev-bound-%d-%d-%d", db, i, cycle)
		if cycle >= 5 && i < changed {
			in.Reason = "blast radius exceeded"
		}
		values, err := decisionValues(in)
		if err != nil {
			t.Fatalf("encode decision %d: %v", i, err)
		}
		values[fingerprintParam] = in.Fingerprint
		batch.Queue(upsertDecisionSQL, values...)
	}
	results := pool.SendBatch(ctx, batch)
	for i := 0; i < n; i++ {
		var id int64
		var evidenceID string
		if err := results.QueryRow().Scan(&id, &evidenceID); err != nil {
			_ = results.Close()
			t.Fatalf("cycle %d upsert %d: %v", cycle, i, err)
		}
	}
	if err := results.Close(); err != nil {
		t.Fatalf("cycle %d: %v", cycle, err)
	}
}
