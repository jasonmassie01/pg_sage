//go:build integration

package plan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxLockRetries bounds how often the runner retries a step whose lock
// wait hit lock_timeout, the way an operator would rerun it.
const maxLockRetries = 40

// swapRun is what running a plan cost: the time each lock-only step (and
// each swap transaction attempt) took, the backfill batches and retries.
type swapRun struct {
	lockWaits map[string]time.Duration
	batches   int
	retries   int
}

// workSteps scan or build; their run time is work, not a lock wait.
var workSteps = map[string]bool{
	"backfill": true, "validate_not_null_proof": true, "build_unique_index": true,
	"validate_referencing_fk": true,
}

// runSwapScript runs every step of p on one session, in order, as the
// script tells an operator to: the backfill until it returns no rows, the
// swap transaction as one unit, and a step that timed out on a lock again.
func runSwapScript(t *testing.T, pool *pgxpool.Pool, p SwapPlan) swapRun {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire script session: %v", err)
	}
	defer conn.Release()
	run := swapRun{lockWaits: map[string]time.Duration{}}
	for i := 0; i < len(p.Steps); i++ {
		s := p.Steps[i]
		switch {
		case s.Name == "begin":
			end := i
			for p.Steps[end].Name != "commit" {
				end++
			}
			runSwapTransaction(t, conn, p.Steps[i:end+1], &run)
			i = end
		case s.Repeat:
			run.batches += runBackfill(t, conn, s)
		default:
			runOneStep(t, conn, s, &run)
		}
	}
	return run
}

func lockTimedOut(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "40P01")
}

func runOneStep(t *testing.T, conn *pgxpool.Conn, s SwapStep, run *swapRun) {
	t.Helper()
	ctx := context.Background()
	for attempt := 0; ; attempt++ {
		start := time.Now()
		_, err := conn.Exec(ctx, s.SQL)
		if d := time.Since(start); !workSteps[s.Name] && d > run.lockWaits[s.Name] {
			run.lockWaits[s.Name] = d
		}
		if err == nil {
			return
		}
		if !lockTimedOut(err) || attempt >= maxLockRetries {
			t.Fatalf("step %s %q: %v", describeStep(s), s.SQL, err)
		}
		run.retries++
		if s.Name == "build_unique_index" {
			// A failed CREATE INDEX CONCURRENTLY leaves an invalid index.
			dropInvalidIndex(t, conn, s)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func dropInvalidIndex(t *testing.T, conn *pgxpool.Conn, s SwapStep) {
	t.Helper()
	if _, err := conn.Exec(context.Background(),
		`DROP INDEX CONCURRENTLY IF EXISTS swap09b.orders_id_swap_key`); err != nil {
		t.Fatalf("drop invalid index after %s failed: %v", s.Name, err)
	}
}

// runSwapTransaction runs BEGIN..COMMIT; a lock timeout rolls the whole
// transaction back and it runs again from BEGIN.
func runSwapTransaction(t *testing.T, conn *pgxpool.Conn, steps []SwapStep, run *swapRun) {
	t.Helper()
	ctx := context.Background()
	for attempt := 0; ; attempt++ {
		start := time.Now()
		failed, err := execSteps(ctx, conn, steps)
		if d := time.Since(start); d > run.lockWaits["swap_transaction"] {
			run.lockWaits["swap_transaction"] = d
		}
		if err == nil {
			return
		}
		if _, rbErr := conn.Exec(ctx, "ROLLBACK"); rbErr != nil {
			t.Fatalf("rollback swap transaction: %v", rbErr)
		}
		if !lockTimedOut(err) || attempt >= maxLockRetries {
			t.Fatalf("swap step %s %q: %v", describeStep(failed), failed.SQL, err)
		}
		run.retries++
		time.Sleep(100 * time.Millisecond)
	}
}

func execSteps(ctx context.Context, conn *pgxpool.Conn, steps []SwapStep) (SwapStep, error) {
	for _, s := range steps {
		if _, err := conn.Exec(ctx, s.SQL); err != nil {
			return s, err
		}
	}
	return SwapStep{}, nil
}

// runBackfill runs the batch with $1 = the last key returned, starting
// from NULL, until a batch returns no rows; it returns the batch count.
func runBackfill(t *testing.T, conn *pgxpool.Conn, s SwapStep) int {
	t.Helper()
	ctx := context.Background()
	var last *int64
	batches := 0
	for retries := 0; ; {
		rows, err := conn.Query(ctx, s.SQL, last)
		var keys []int64
		if err == nil {
			for rows.Next() {
				var k int64
				if err = rows.Scan(&k); err != nil {
					break
				}
				keys = append(keys, k)
			}
			rows.Close()
			if err == nil {
				err = rows.Err()
			}
		}
		if err != nil {
			if !lockTimedOut(err) || retries >= maxLockRetries {
				t.Fatalf("backfill batch %d: %v", batches, err)
			}
			retries++
			continue
		}
		if len(keys) == 0 {
			return batches
		}
		batches++
		maxKey := keys[0]
		for _, k := range keys {
			if k > maxKey {
				maxKey = k
			}
		}
		last = &maxKey
	}
}
