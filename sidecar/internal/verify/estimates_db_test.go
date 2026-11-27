package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Row-estimate error measured from real plans: two perfectly correlated
// columns make the planner multiply their selectivities (1% x 1%) and
// underestimate a two-column predicate about 100x; CREATE STATISTICS
// (dependencies) plus ANALYZE fixes it. The verifier must see exactly that
// in plans stored the way pg_sage stores them (sage.explain_cache).

const corrQuery = "SELECT * FROM %s WHERE a = 1 AND b = 1"

func corrTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	table := fmt.Sprintf("public.vstat_%d", time.Now().UnixNano())
	mustExec(t, ctx, pool, "CREATE TABLE "+table+" (a int, b int)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})
	mustExec(t, ctx, pool, "INSERT INTO "+table+
		" SELECT i % 100, i % 100 FROM generate_series(1, 20000) i")
	mustExec(t, ctx, pool, "ANALYZE "+table)
	return table
}

// capturePlans stores n EXPLAIN (ANALYZE, FORMAT JSON) plans of the
// correlated query as qid's, captured at at.
func capturePlans(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string,
	qid int64, at time.Time, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		var plan string
		if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+
			fmt.Sprintf(corrQuery, table)).Scan(&plan); err != nil {
			t.Fatalf("explain analyze: %v", err)
		}
		mustExec(t, ctx, pool, `INSERT INTO sage.explain_cache
			(captured_at, queryid, query_text, plan_json, source)
			VALUES ($1, $2, $3, $4::jsonb, 'auto_explain_log')`,
			at.Add(time.Duration(i)*time.Second), qid, fmt.Sprintf(corrQuery, table), plan)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.explain_cache WHERE queryid = $1", qid)
	})
}

func TestEstimateErrorsSeeCreateStatisticsFixTheEstimate(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	table := corrTable(t, ctx, pool)
	qid := int64(990_300_000) + time.Now().UnixNano()%100_000
	start := time.Now().Add(-3 * time.Hour).UTC()
	capturePlans(t, ctx, pool, table, qid, start, 3)
	mustExec(t, ctx, pool, "CREATE STATISTICS "+strings.TrimPrefix(table, "public.")+
		"_ab (dependencies) ON a, b FROM "+table)
	mustExec(t, ctx, pool, "ANALYZE "+table)
	after := time.Now().UTC()
	capturePlans(t, ctx, pool, table, qid, after, 3)

	src := NewPostgresObservationSource(pool)
	before, err := src.EstimateErrors(ctx, []int64{qid}, start.Add(-time.Minute),
		start.Add(time.Hour))
	if err != nil {
		t.Fatalf("EstimateErrors(before): %v", err)
	}
	now, err := src.EstimateErrors(ctx, []int64{qid}, after.Add(-time.Second),
		after.Add(time.Minute))
	if err != nil {
		t.Fatalf("EstimateErrors(after): %v", err)
	}
	if before.Plans != 3 || before.QError < 20 {
		t.Fatalf("before = %+v, want 3 plans with the ~100x misestimate", before)
	}
	if now.Plans != 3 || now.QError > 2 {
		t.Fatalf("after = %+v, want 3 plans with an accurate estimate", now)
	}
	if j := JudgeEstimates(before, now, 3); j.Verdict != OutcomeImproved {
		t.Fatalf("judgement = %+v, want improved", j)
	}
}

// Only the targets' plans with actual rows, inside the window, count.
func TestEstimateErrorsIgnoresOtherPlans(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	qid := int64(990_400_000) + time.Now().UnixNano()%100_000
	at := time.Now().Add(-time.Hour).UTC()
	analyzed := `[{"Plan":{"Node Type":"Seq Scan","Plan Rows":1,"Actual Rows":64,
		"Actual Loops":1}}]`
	rows := []struct {
		qid  int64
		at   time.Time
		plan string
	}{
		{qid, at, analyzed},
		{qid, at.Add(time.Minute), `[{"Plan":{"Node Type":"Seq Scan","Plan Rows":5}}]`},
		{qid, at.Add(-2 * time.Hour), analyzed},
		{qid + 1, at, analyzed},
	}
	for _, r := range rows {
		mustExec(t, ctx, pool, `INSERT INTO sage.explain_cache (captured_at, queryid,
			plan_json, source) VALUES ($1, $2, $3::jsonb, 'collector')`, r.at, r.qid, r.plan)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.explain_cache WHERE queryid IN ($1, $2)", qid, qid+1)
	})
	got, err := NewPostgresObservationSource(pool).EstimateErrors(ctx, []int64{qid},
		at.Add(-time.Minute), at.Add(time.Hour))
	if err != nil {
		t.Fatalf("EstimateErrors: %v", err)
	}
	if got.Plans != 1 || got.QError != 64 {
		t.Fatalf("got %+v, want the one analyzed plan in the window (q 64)", got)
	}
	empty, err := NewPostgresObservationSource(pool).EstimateErrors(ctx, nil, at, at)
	if err != nil || empty.Plans != 0 {
		t.Fatalf("no targets = %+v, %v; want an empty sample", empty, err)
	}
}

func TestEstimateErrorsWithoutPool(t *testing.T) {
	_, err := (&PostgresObservationSource{}).EstimateErrors(context.Background(),
		[]int64{1}, time.Now(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("err = %v, want the pool-unavailable error", err)
	}
}

// The footprint of a REINDEX target: an index's size and validity, or the
// sum over a table's indexes (valid only if all are). The rebuilt index
// keeps every deleted row the VACUUM before it could not remove, so the
// test holds the exclusive server: on the shared one, PostgreSQL 14 counts
// a backend still logging in to any database in this database's horizon,
// with a snapshot reaching back to the oldest transaction running there,
// and no view lists it (CI, PG14: 1138688 -> 1138688 bytes, neutral).
func TestIndexFootprintBeforeAndAfterReindex(t *testing.T) {
	checkReindexFootprint(t, verifyExclusivePool(t,
		"the VACUUM before a REINDEX must face no snapshot older than its deletes"))
}

// checkReindexFootprint bloats an index, reindexes it and checks the
// footprint before and after on pool.
func checkReindexFootprint(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := t.Context()
	table := fmt.Sprintf("vreidx_%d", time.Now().UnixNano())
	mustExec(t, ctx, pool, "CREATE TABLE public."+table+" (a int)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	mustExec(t, ctx, pool, "CREATE INDEX "+table+"_a ON public."+table+" (a)")
	mustExec(t, ctx, pool, "INSERT INTO public."+table+
		" SELECT i FROM generate_series(1, 50000) i")
	mustExec(t, ctx, pool, "DELETE FROM public."+table+" WHERE a % 10 <> 0")
	waitSnapshotsPastNow(t, ctx, pool)
	mustExec(t, ctx, pool, "VACUUM public."+table)
	src := NewPostgresObservationSource(pool)
	before, valid, err := src.IndexFootprint(ctx, "public."+table+"_a", false)
	if err != nil || !valid || before <= 0 {
		t.Fatalf("before = %d valid=%v err=%v", before, valid, err)
	}
	mustExec(t, ctx, pool, "REINDEX INDEX public."+table+"_a")
	after, valid, err := src.IndexFootprint(ctx, "public."+table+"_a", false)
	if err != nil || !valid {
		t.Fatalf("after: valid=%v err=%v", valid, err)
	}
	if j := JudgeIndexSize(before, after, valid); j.Verdict != OutcomeImproved {
		t.Fatalf("bloated index %d -> %d bytes judged %+v, want improved", before, after, j)
	}
	total, valid, err := src.IndexFootprint(ctx, "public."+table, true)
	if err != nil || !valid || total != after {
		t.Fatalf("table footprint = %d valid=%v err=%v, want %d", total, valid, err, after)
	}
	if _, _, err := src.IndexFootprint(ctx, "public.no_such_index_xyz", false); err == nil {
		t.Fatal("a missing index must be an error, not a zero size")
	}
}

// A failed CREATE UNIQUE INDEX CONCURRENTLY leaves an invalid index: the
// footprint reports it invalid (a REINDEX that leaves one is a regression).
func TestIndexFootprintReportsAnInvalidIndex(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	table := fmt.Sprintf("vinv_%d", time.Now().UnixNano())
	mustExec(t, ctx, pool, "CREATE TABLE public."+table+" (a int)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	mustExec(t, ctx, pool, "INSERT INTO public."+table+" VALUES (1), (1)")
	if _, err := pool.Exec(ctx, "CREATE UNIQUE INDEX CONCURRENTLY "+table+
		"_u ON public."+table+" (a)"); err == nil {
		t.Fatal("unique index over duplicates must fail")
	}
	_, valid, err := NewPostgresObservationSource(pool).IndexFootprint(ctx,
		"public."+table+"_u", false)
	if err != nil || valid {
		t.Fatalf("valid=%v err=%v, want an invalid index", valid, err)
	}
	_, valid, err = NewPostgresObservationSource(pool).IndexFootprint(ctx,
		"public."+table, true)
	if err != nil || valid {
		t.Fatalf("table with an invalid index: valid=%v err=%v", valid, err)
	}
}

// waitSnapshotsPastNow waits until no other backend holds a snapshot or a
// transaction id older than this moment (snapshotsPastNowSQL). On the
// shared CI server another package's open transaction kept the deleted rows
// visible, so neither VACUUM nor REINDEX could drop them and the "bloated"
// index kept its size (PG14 CI: neutral).
func waitSnapshotsPastNow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var now string
	if err := pool.QueryRow(ctx, "SELECT pg_current_xact_id()::text").Scan(&now); err != nil {
		t.Fatalf("read the current transaction id: %v", err)
	}
	for i := 0; i < 120; i++ {
		clear, err := snapshotsPastNow(ctx, pool, now)
		if err != nil {
			t.Fatalf("read the xmin horizon: %v", err)
		}
		if clear {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("another backend held a snapshot older than the test's deletes for 30 s")
}

// snapshotsPastNowSQL is true when no other backend that can hold back a
// VACUUM or REINDEX of this database's tables holds a snapshot or a
// transaction id older than the transaction id $1. Those are the backends
// of this database and the ones bound to none (a walsender with
// hot_standby_feedback); PostgreSQL ignores other databases' snapshots for
// ordinary tables, so the shared test server's other packages never count
// (ci-17). A writer idle in its transaction holds no snapshot, only its
// transaction id. A backend that pg_stat_activity does not list yet but
// whose transaction holds its virtualxid lock is still logging in:
// PostgreSQL 14 counts it in every database's horizon, with a snapshot
// that reaches back to the oldest transaction running in any database.
const snapshotsPastNowSQL = `SELECT NOT EXISTS (SELECT 1 FROM pg_stat_activity
		WHERE pid <> pg_backend_pid() AND (datid IS NULL OR datname = current_database())
		  AND (age(backend_xmin) >= age(xid($1::xid8))
		    OR age(backend_xid) >= age(xid($1::xid8))))
	AND NOT EXISTS (SELECT 1 FROM pg_locks l
		WHERE l.locktype = 'virtualxid' AND l.pid <> pg_backend_pid()
		  AND NOT EXISTS (SELECT 1 FROM pg_stat_activity a WHERE a.pid = l.pid))`

// snapshotsPastNow reports whether snapshotsPastNowSQL holds for the
// transaction id now.
func snapshotsPastNow(ctx context.Context, pool *pgxpool.Pool, now string) (bool, error) {
	if strings.TrimSpace(now) == "" {
		// PG 14 reads '' as transaction id 0, which every snapshot is past.
		return false, errors.New("no transaction id to compare snapshots with")
	}
	var clear bool
	err := pool.QueryRow(ctx, snapshotsPastNowSQL, now).Scan(&clear)
	return clear, err
}
