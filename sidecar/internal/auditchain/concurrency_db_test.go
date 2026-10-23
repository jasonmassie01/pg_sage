package auditchain_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Concurrent writers, including transactions that update one row and then
// insert another (the order that deadlocks an eagerly-locked chain), lose
// no link and fork nothing.
func TestConcurrentWritersLoseNoLinks(t *testing.T) {
	pool := freshDB(t, "chain_concurrent")
	ctx := context.Background()
	seed := make([]int64, 8)
	for i := range seed {
		seed[i] = insertAction(t, pool, fmt.Sprintf("CREATE INDEX seed%d", i))
	}
	const writers, perWriter = 8, 20
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				errs <- writeMixed(ctx, pool, seed[(w+i)%len(seed)], w, i)
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write failed: %v", err)
		}
	}
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	// The seed inserts, then one state update and one insert per write.
	want := len(seed) + 2*writers*perWriter
	if !rep.OK() || rep.Links != want || rep.HeadSeq != int64(want) {
		t.Fatalf("after %d concurrent writes: links=%d head=%d problems=%+v",
			writers*perWriter, rep.Links, rep.HeadSeq, rep.Problems)
	}
}

// writeMixed updates another row's outcome, then inserts a row, in one
// transaction. The outcome always changes, so every update is a link.
func writeMixed(ctx context.Context, pool *pgxpool.Pool, other int64, w, i int) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE sage.action_log SET outcome = $1 WHERE id = $2`,
		fmt.Sprintf("w%d-%d", w, i), other); err != nil {
		return fmt.Errorf("update %d: %w", other, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('vacuum', $1)`, fmt.Sprintf("VACUUM w%d_%d", w, i)); err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	return tx.Commit(ctx)
}

// The statements the chain runs on every write find their rows through an
// index (perf gate A): the head probe and the per-row link lookup.
func TestChainStatementsUseIndexes(t *testing.T) {
	pool := freshDB(t, "chain_plans")
	exec(t, pool, `INSERT INTO sage.audit_chain_link (chain, seq, row_id, op, v,
		sealed_hash, state, prev_hash, hash)
		SELECT 'perf', g, g, 'I', 1, '', '', '', '' FROM generate_series(1, 20000) g`)
	exec(t, pool, "ANALYZE sage.audit_chain_link")
	ctx := context.Background()
	for _, sql := range []string{auditchain.HeadSQL, auditchain.RowLinksSQL} {
		args := []any{"perf"}
		if sql == auditchain.RowLinksSQL {
			args = append(args, int64(77))
		}
		plan, err := testdb.Explain(ctx, pool, "", sql, args...)
		if err != nil {
			t.Fatalf("explain %s: %v", sql, err)
		}
		if n := plan.SeqScans("audit_chain_link"); n != 0 {
			t.Fatalf("%s plans a sequential scan of audit_chain_link:\n%s", sql, plan)
		}
	}
}
