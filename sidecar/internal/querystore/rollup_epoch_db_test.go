package querystore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// evid-01a: epoch breaks (the spec's Neon-like suspend check), two
// rollers at once, and the error paths, on a real server.

const resetMarker = "rollup_reset_workload"

// readWorkload reads the workload statement's counters and the statements
// epoch the way the collector does (stats_reset, or the postmaster start).
func readWorkload(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (qc, time.Time) {
	t.Helper()
	var q qc
	var epoch time.Time
	err := pool.QueryRow(ctx, `SELECT s.queryid, sum(s.calls)::int8,
		sum(s.total_exec_time)::float8, sum(s.rows)::int8, sum(s.shared_blks_hit)::int8,
		sum(s.shared_blks_read)::int8, sum(s.temp_blks_read)::int8,
		sum(s.temp_blks_written)::int8, COALESCE(sum(s.wal_bytes), 0)::int8,
		(SELECT GREATEST(i.stats_reset, pg_postmaster_start_time())
		   FROM pg_stat_statements_info i)
		FROM pg_stat_statements s
		WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND strpos(s.query, $1) > 0 AND s.query NOT LIKE '%pg_stat_statements%'
		GROUP BY s.queryid`, resetMarker).Scan(&q.id, &q.calls, &q.exec, &q.rows, &q.hit,
		&q.read, &q.tread, &q.twrite, &q.wal, &epoch)
	if err != nil {
		t.Fatalf("read the workload's counters: %v", err)
	}
	return q, epoch
}

func runWorkload(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	for range n {
		if _, err := pool.Exec(ctx, "SELECT count(*) AS "+resetMarker+
			" FROM generate_series(1, 100)"); err != nil {
			t.Fatalf("workload: %v", err)
		}
	}
}

func TestRollupHour_StatementsResetIsAnEpochBreak(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	ctx := context.Background()
	if _, err := p.Monitored.Exec(ctx,
		"CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		t.Fatalf("pg_stat_statements: %v", err)
	}
	if _, err := testdb.ResetStatements(ctx, p.Monitored); err != nil {
		t.Fatal(err)
	}
	hour, w := pastHour(2), snapstore.NewWriter()
	snap := func(at time.Duration) {
		q, epoch := readWorkload(t, ctx, p.Monitored)
		persistCycle(t, w, p.Monitored, hour.Add(at), []qc{q}, sys{epoch: &epoch})
	}
	runWorkload(t, ctx, p.Monitored, 2)
	snap(5 * time.Minute)
	runWorkload(t, ctx, p.Monitored, 3)
	snap(10 * time.Minute)
	// A Neon suspend drops the statistics the same way.
	if _, err := testdb.ResetStatements(ctx, p.Monitored); err != nil {
		t.Fatal(err)
	}
	runWorkload(t, ctx, p.Monitored, 2)
	snap(20 * time.Minute)
	runWorkload(t, ctx, p.Monitored, 3)
	snap(30 * time.Minute)
	rep, err := RollupHour(ctx, p.Monitored, hour)
	if err != nil {
		t.Fatal(err)
	}
	rows := readHourRows(t, p.Monitored, hour)
	if len(rows) != 1 || rep.EpochBreaks != 1 {
		t.Fatalf("rows %+v, report %+v: want the workload with one break", rows, rep)
	}
	for _, r := range rows {
		if !r.brk || r.calls != 6 || r.pairs != 2 {
			t.Fatalf("row = %+v, want epoch_break, calls 3 + 3 and 2 pairs", r)
		}
		if r.exec < 0 || r.rows < 0 || r.hit < 0 || r.read < 0 || r.wal < 0 {
			t.Fatalf("a counter is negative: %+v", r)
		}
	}
	if c, _ := readCoverage(t, p.Monitored, hour); c.breaks != 1 {
		t.Fatalf("coverage epoch_breaks = %d, want 1", c.breaks)
	}
}

func TestRollupHour_RecordedEpochChangeBreaksRegrownCounters(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	hour, w := pastHour(2), snapstore.NewWriter()
	e1, e2 := pastHour(30), hour.Add(15*time.Minute)
	persistCycle(t, w, p.Monitored, hour.Add(10*time.Minute),
		[]qc{{id: 9, calls: 100, exec: 100}}, sys{epoch: &e1})
	persistCycle(t, w, p.Monitored, hour.Add(20*time.Minute),
		[]qc{{id: 9, calls: 150, exec: 150}}, sys{epoch: &e2})
	persistCycle(t, w, p.Monitored, hour.Add(30*time.Minute),
		[]qc{{id: 9, calls: 160, exec: 152}}, sys{epoch: &e2})
	if _, err := RollupHour(context.Background(), p.Monitored, hour); err != nil {
		t.Fatal(err)
	}
	r := readHourRows(t, p.Monitored, hour)[9]
	if !r.brk || r.calls != 10 || r.exec != 2 || r.pairs != 1 {
		t.Fatalf("row = %+v; the regrown pair must break, only 160-150 counts", r)
	}
}

func TestRollupHour_TwoRollersLeaveOneRowEach(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	start := writeThreeHours(t, p)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 2*24)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range 24 {
				_, err := RollupHour(ctx, p.Monitored, start.Add(time.Duration(h-20)*time.Hour))
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent roll up: %v", err)
		}
	}
	var rows, keys, hours int
	if err := p.Monitored.QueryRow(ctx, `SELECT count(*), count(DISTINCT (queryid, hour)),
		(SELECT count(*) FROM sage.query_rollup_coverage) FROM sage.query_rollup_hourly`).
		Scan(&rows, &keys, &hours); err != nil {
		t.Fatal(err)
	}
	if rows != 3*fixtureQueryIDs || keys != rows || hours != 24 {
		t.Fatalf("%d rows, %d keys, %d coverage rows; want 60, 60, 24", rows, keys, hours)
	}
	for k := range 3 {
		checkReconciled(t, readHourRows(t, p.Monitored, start.Add(time.Duration(k)*time.Hour)),
			k)
	}
}

func TestRollupHour_UnregisteredPoolInMetaModeWritesNothing(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	if err := histstore.SetProcessMode(histstore.ModeMeta); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = histstore.SetProcessMode(histstore.ModeMonitored) }()
	_, err := RollupHour(context.Background(), p.Monitored, pastHour(2))
	if !errors.Is(err, histstore.ErrNotRegistered) {
		t.Fatalf("err = %v, want histstore.ErrNotRegistered", err)
	}
	for _, pool := range []*pgxpool.Pool{p.Monitored, p.Meta} {
		var n int
		if err := pool.QueryRow(context.Background(),
			"SELECT count(*) FROM sage.query_rollup_coverage").Scan(&n); err != nil || n != 0 {
			t.Fatalf("a refused roll up wrote %d coverage rows (%v)", n, err)
		}
	}
}

func TestRollupHour_StatementTimeoutNamesTheBound(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	ctx := context.Background()
	old := rollupStatementTimeout
	rollupStatementTimeout = 300 * time.Millisecond
	defer func() { rollupStatementTimeout = old }()
	holder, err := p.Monitored.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('sage.query_rollup'),
		0)`); err != nil {
		t.Fatal(err)
	}
	_, err = RollupHour(ctx, p.Monitored, pastHour(2))
	if err == nil || !strings.Contains(err.Error(), "statement_timeout 300ms") {
		t.Fatalf("err = %v, want the 300ms statement bound named", err)
	}
}
