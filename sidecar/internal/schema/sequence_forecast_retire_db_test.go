package schema

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// forecast-05: the forecaster's sequence rule is retired; Bootstrap
// resolves its open forecast_sequence_exhaustion findings once, naming
// forecast_sequence_runway as the successor. Other findings are left
// alone, and a run with nothing open changes nothing.

// No concurrent-writer test beyond the lock wait below: Bootstrap holds
// its advisory lock, so one migration runs per database at a time.

type findingState struct {
	status   string
	resolved *time.Time
	reason   *string
}

func seedFinding(t *testing.T, pool *pgxpool.Pool, cat, ident, status string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, status,
		 resolved_at)
		VALUES ($1, 'warning', 'sequence', $2, 'seeded', '{"current_pct": 97}', $3,
		        CASE WHEN $3 = 'resolved' THEN now() - interval '3 days' END)
		RETURNING id`, cat, ident, status).Scan(&id); err != nil {
		t.Fatalf("seed %s finding: %v", cat, err)
	}
	return id
}

func stateOf(t *testing.T, pool *pgxpool.Pool, id int64) findingState {
	t.Helper()
	var s findingState
	if err := pool.QueryRow(context.Background(), `SELECT status, resolved_at,
		detail->>'resolved_reason' FROM sage.findings WHERE id = $1`, id).
		Scan(&s.status, &s.resolved, &s.reason); err != nil {
		t.Fatalf("read finding %d: %v", id, err)
	}
	return s
}

func TestRetireSequenceForecast_ResolvesOpenFindingsOnce(t *testing.T) {
	pool := freshSchemaPool(t, "seqretire")
	ctx := context.Background()
	open := seedFinding(t, pool, retiredSequenceCategory, "public.orders_id_seq", "open")
	old := seedFinding(t, pool, retiredSequenceCategory, "public.old_seq", "resolved")
	runway := seedFinding(t, pool, "forecast_sequence_runway", "public.orders_id_seq",
		"open")
	oldBefore := stateOf(t, pool, old)
	if err := retireSequenceForecast(ctx, pool); err != nil {
		t.Fatalf("first run: %v", err)
	}
	got := stateOf(t, pool, open)
	if got.status != "resolved" || got.resolved == nil || got.reason == nil ||
		*got.reason != "superseded by forecast_sequence_runway" {
		t.Fatalf("open finding after the migration = %+v", got)
	}
	if s := stateOf(t, pool, runway); s.status != "open" || s.resolved != nil {
		t.Fatalf("the runway's finding was touched: %+v", s)
	}
	if s := stateOf(t, pool, old); !s.resolved.Equal(*oldBefore.resolved) || s.reason != nil {
		t.Fatalf("an already resolved finding was rewritten: %+v", s)
	}
	if err := retireSequenceForecast(ctx, pool); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again := stateOf(t, pool, open); !again.resolved.Equal(*got.resolved) ||
		again.status != "resolved" {
		t.Fatalf("the second run changed the resolved finding: %+v -> %+v", got, again)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap after the migration: %v", err)
	}
}

// Bootstrap itself runs the step: a finding open before it is resolved.
func TestBootstrap_RetiresTheSequenceForecast(t *testing.T) {
	pool := freshSchemaPool(t, "seqretireboot")
	id := seedFinding(t, pool, retiredSequenceCategory, "public.a_seq", "open")
	if err := Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if s := stateOf(t, pool, id); s.status != "resolved" {
		t.Fatalf("finding after bootstrap = %+v, want resolved", s)
	}
}

// A finding locked by another session defers the migration (lock
// timeout) without failing the bootstrap; the next run resolves it.
func TestRetireSequenceForecast_DefersOnALockTimeout(t *testing.T) {
	pool := freshSchemaPool(t, "seqretirelock")
	ctx := context.Background()
	id := seedFinding(t, pool, retiredSequenceCategory, "public.locked_seq", "open")
	prev := retireSequenceLockTimeout
	retireSequenceLockTimeout = 200 * time.Millisecond
	t.Cleanup(func() { retireSequenceLockTimeout = prev })
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM sage.findings WHERE id = $1 FOR UPDATE",
		id); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := retireSequenceForecast(ctx, pool); err != nil {
		t.Fatalf("a lock timeout failed the migration: %v", err)
	}
	if s := stateOf(t, pool, id); s.status != "open" {
		t.Fatalf("locked finding = %+v, want it still open", s)
	}
	_ = tx.Rollback(ctx)
	if err := retireSequenceForecast(ctx, pool); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if s := stateOf(t, pool, id); s.status != "resolved" {
		t.Fatalf("after the lock was released = %+v, want resolved", s)
	}
}
