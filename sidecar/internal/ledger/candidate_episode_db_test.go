package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// policy-02, error propagation: the execute record and the resolution of
// the candidate's open rows are one statement. When the resolution cannot
// run (here: a row lock it cannot take within lock_timeout), the execute
// record fails and nothing of it is committed.
func TestResolveFailureFailsTheExecuteRecord(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	service := NewService(NewPostgresRepository(pool))
	park := parkedInput(fmt.Sprintf("lockd_%d", time.Now().UnixNano()), ledgerDatabaseID())
	key := CandidateKey(park)
	cleanupCandidate(t, pool, key)
	parked, err := service.RecordDecision(ctx, park)
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `SELECT 1 FROM sage.decision WHERE id = $1 FOR UPDATE`,
		parked.ID); err != nil {
		t.Fatalf("hold the park's row lock: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '100ms'`); err != nil {
		t.Fatalf("lock_timeout: %v", err)
	}
	execute := asExecute(park)
	execute.EvidenceID = ledgerEvidenceID("ev-lockd")
	_, err = NewService(NewTxRepository(tx)).RecordDecision(ctx, execute)
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("execute while the park is locked: err = %v, want lock_not_available", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the holder: %v", err)
	}
	rows := candidateRows(t, pool, key)
	if len(rows) != 1 || rows[0].ID != parked.ID || rows[0].Resolved {
		t.Fatalf("rows = %+v, want only park %d, still open", rows, parked.ID)
	}
}

// Concurrent access: repeats racing an execute never leave two open rows
// of one fingerprint (the partial unique index) and never fail.
func TestConcurrentParksAndAnExecuteKeepOneOpenRow(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	service := NewService(NewPostgresRepository(pool))
	park := parkedInput(fmt.Sprintf("race_%d", time.Now().UnixNano()), ledgerDatabaseID())
	key := CandidateKey(park)
	cleanupCandidate(t, pool, key)
	var wg sync.WaitGroup
	errs := make(chan error, 9)
	for i := 0; i < 9; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			input := park
			if i == 4 {
				input = asExecute(park)
			}
			_, err := service.RecordDecision(ctx, input)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent record: %v", err)
		}
	}
	open, repeats, executes := 0, 0, 0
	for _, row := range candidateRows(t, pool, key) {
		if row.Verdict == "execute" {
			executes++
			continue
		}
		repeats += row.Repeats
		if !row.Resolved {
			open++
		}
	}
	if executes != 1 || repeats != 8 || open > 1 {
		t.Fatalf("executes=%d park repeats=%d open parks=%d, want 1, 8, at most 1",
			executes, repeats, open)
	}
}

// Error propagation: a failed link is returned with its context.
func TestLinkQueueReportsAFailure(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	linked, err := NewPostgresRepository(pool).LinkQueue(cancelled, 1, 1)
	if err == nil || linked || !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "link decision 1 to queue item 1") {
		t.Fatalf("link on a cancelled context = %v, %v, want a wrapped cancel", linked, err)
	}
}
