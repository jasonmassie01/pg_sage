//go:build integration

package plan

import (
	"context"
	"testing"
	"time"
)

// A transaction that holds a conflicting lock makes the script's lock
// waits fail at lock_timeout instead of queueing behind it: every attempt
// ends within lock_timeout, the runner retries, and the swap completes
// once the holder commits.
func TestColumnSwapLockWaitStopsAtLockTimeout(t *testing.T) {
	pool := planDBPool(t)
	swapSchema(t, pool,
		`CREATE TABLE swap09b.orders (id serial PRIMARY KEY, v text NOT NULL)`,
		`INSERT INTO swap09b.orders (v) SELECT 'v' FROM generate_series(1, 10000)`)
	facts, err := readFacts(t, pool, "orders", "id")
	if err != nil {
		t.Fatalf("ReadSwapFacts: %v", err)
	}
	const timeout = 300 * time.Millisecond
	p, err := PlanColumnSwap(context.Background(), facts, SwapRequest{Schema: "swap09b",
		Table: "orders", Column: "id", TargetType: "bigint", LockTimeout: timeout})
	if err != nil {
		t.Fatalf("PlanColumnSwap: %v", err)
	}
	ctx := context.Background()
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	if _, err := holder.Exec(ctx, `UPDATE swap09b.orders SET v = 'held' WHERE id = 1`); err != nil {
		t.Fatalf("holder update: %v", err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		released <- holder.Commit(ctx)
	}()
	run := runSwapScript(t, pool, p)
	if err := <-released; err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	if run.retries == 0 {
		t.Fatal("no step hit lock_timeout while a conflicting lock was held")
	}
	for name, d := range run.lockWaits {
		if d > timeout+lockSlack {
			t.Errorf("step %s took %v, past lock_timeout %v", name, d, timeout)
		}
	}
	var typ string
	if err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod) FROM pg_attribute
		WHERE attrelid = 'swap09b.orders'::regclass AND attname = 'id'`).Scan(&typ); err != nil ||
		typ != "bigint" {
		t.Fatalf("id type after the swap = %q (%v), want bigint", typ, err)
	}
}
