package executor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
)

// An operator-approved unused-index drop (ExecuteManual: "Take action",
// approved queue items, durable approvals) is re-checked live like an
// autonomous one: the approval was given on the finding's evidence, and a
// scan or a statistics reset since then invalidates it. The refusal names
// the failed check so the UI can show why (HTTP 409).

func manualUnusedFixture(t *testing.T) (*pgxpool.Pool, *Executor) {
	t.Helper()
	pool := evidencePool(t)
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	e := manualExecutor(pool)
	withTestStandingGate(e)
	return pool, e
}

func unusedFinding(t *testing.T, pool *pgxpool.Pool, category, ident string) (int, string) {
	t.Helper()
	sql := "DROP INDEX CONCURRENTLY " + ident
	var id int
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql, rollback_sql)
		VALUES ($1, 'warning', 'index', $2, 'unused', '{}', 'drop', $3,
		        'CREATE INDEX ev_a ON public.ev_t (a)')
		RETURNING id`, category, ident, sql).Scan(&id); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	return id, sql
}

func regclassExists(t *testing.T, pool *pgxpool.Pool, ident string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT to_regclass($1) IS NOT NULL`, ident).Scan(&ok); err != nil {
		t.Fatalf("regclass: %v", err)
	}
	return ok
}

func assertEvidenceRefusal(t *testing.T, err error, ident string, check string) {
	t.Helper()
	if !errors.Is(err, ErrUnusedEvidenceBroken) {
		t.Fatalf("ExecuteManual = %v, want ErrUnusedEvidenceBroken", err)
	}
	if !strings.Contains(err.Error(), ident) || !strings.Contains(err.Error(), check) {
		t.Fatalf("error %q does not name %s and the failed check %q", err, ident, check)
	}
	if status := ManualExecuteStatus(err); status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
}

// A scan after approval: refused, the index stays.
func TestExecuteManual_UnusedDropRefusedWhenScanned(t *testing.T) {
	pool, e := manualUnusedFixture(t)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off; SET enable_bitmapscan = off;
		SELECT * FROM public.ev_t WHERE a = 7`); err != nil {
		t.Fatalf("scan: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, _ = conn.Exec(ctx, `SELECT 1`) // PG14 sends counters when the backend idles
		ev, err := readUnusedEvidence(ctx, pool, "public.ev_a")
		if err == nil && ev.scans > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scan never visible: %+v (%v)", ev, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	conn.Release()
	id, sql := unusedFinding(t, pool, "unused_index", "public.ev_a")
	_, err = e.ExecuteManual(ctx, id, sql, "", nil)
	assertEvidenceRefusal(t, err, "public.ev_a", "scanned")
	if !regclassExists(t, pool, "public.ev_a") {
		t.Fatal("the index was dropped despite the refusal")
	}
}

// A statistics reset after approval: refused with the reset named.
func TestExecuteManual_UnusedDropRefusedAfterReset(t *testing.T) {
	pool, e := manualUnusedFixture(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `SELECT pg_stat_reset()`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	id, sql := unusedFinding(t, pool, "unused_index", "public.ev_a")
	_, err := e.ExecuteManual(ctx, id, sql, "", nil)
	assertEvidenceRefusal(t, err, "public.ev_a", "statistics reset")
	if !strings.Contains(err.Error(), "7-day") {
		t.Fatalf("error %q does not state the window in days", err)
	}
	var actions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.action_log WHERE finding_id = $1`,
		id).Scan(&actions); err != nil || actions != 0 {
		t.Fatalf("action_log rows = %d (%v), want none for a refused drop", actions, err)
	}
	if !regclassExists(t, pool, "public.ev_a") {
		t.Fatal("the index was dropped despite the refusal")
	}
}

// An index that no longer exists (or never did) is refused, named.
func TestExecuteManual_UnusedDropRefusedWhenIndexGone(t *testing.T) {
	pool, e := manualUnusedFixture(t)
	if _, err := pool.Exec(context.Background(), `CREATE INDEX ev_b ON public.ev_t (a);
		DROP INDEX public.ev_b`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	id, sql := unusedFinding(t, pool, "unused_index", "public.ev_b")
	_, err := e.ExecuteManual(context.Background(), id, sql, "", nil)
	assertEvidenceRefusal(t, err, "public.ev_b", "not found")
}

// Other categories that drop indexes (duplicate_index) do not rest on usage
// counters and are not gated by usage evidence.
func TestExecuteManual_DuplicateDropNotGatedByUsageEvidence(t *testing.T) {
	pool, e := manualUnusedFixture(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `SELECT pg_stat_reset()`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	id, sql := unusedFinding(t, pool, "duplicate_index", "public.ev_a")
	_, err := e.ExecuteManual(ctx, id, sql, "", nil)
	if errors.Is(err, ErrUnusedEvidenceBroken) {
		t.Fatalf("duplicate_index drop refused on usage evidence: %v", err)
	}
}

// The status mapping: a broken evidence refusal is a conflict.
func TestManualExecuteStatus_UnusedEvidence(t *testing.T) {
	err := unusedEvidenceError("public.ix", "index was scanned (3 scans)")
	if ManualExecuteStatus(err) != http.StatusConflict || !errors.Is(err, ErrUnusedEvidenceBroken) {
		t.Fatalf("status = %d for %v, want 409", ManualExecuteStatus(err), err)
	}
	if !strings.Contains(err.Error(), "public.ix") || !strings.Contains(err.Error(), "scanned") {
		t.Fatalf("message %q", err)
	}
}
