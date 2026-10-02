package collector

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/querystore"
	"github.com/pg-sage/sidecar/internal/schema"
)

// No concurrent fixture writes: these probes compare sequential observation epochs.
func preflightPool(t *testing.T) (*pgxpool.Pool, context.Context, *Collector) {
	t.Helper()
	return preflightPoolWith(t, nil)
}

// preflightPoolWith is preflightPool with session GUCs on every pooled
// connection. Adapted setup: the fixture database is shared by every test
// in this package, so each test starts with an empty sage.query_store;
// otherwise samples of the same statement recorded by an earlier test
// fall inside this test's window and change its arithmetic.
func preflightPoolWith(
	t *testing.T, params map[string]string,
) (*pgxpool.Pool, context.Context, *Collector) {
	t.Helper()
	ctx := context.Background()
	pcfg, err := pgxpool.ParseConfig(os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range params {
		pcfg.ConnConfig.RuntimeParams[k] = v
	}
	p, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err = p.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err = schema.Bootstrap(ctx, p); err != nil {
		t.Fatal(err)
	}
	preflightExec(t, p, "DELETE FROM sage.query_store", 1)
	cfg := config.DefaultConfig()
	cfg.Advisor.Enabled = false
	cfg.HasPlanTimeColumns = true
	cfg.Safety.QueryTimeoutMs = 5000
	c := New(p, cfg, 170000, func(level, format string, args ...any) { t.Logf(format, args...) })
	return p, ctx, c
}

func preflightExec(t *testing.T, p *pgxpool.Pool, sql string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := p.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}
}

func preflightCollect(t *testing.T, c *Collector) *Snapshot {
	t.Helper()
	s, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func preflightWorkload(t *testing.T, s *Snapshot) QueryStats {
	t.Helper()
	for _, q := range s.Queries {
		if strings.Contains(q.Query, "sum(i)") {
			return q
		}
	}
	t.Fatal("real workload query missing from collector")
	return QueryStats{}
}

func TestPreflightEvidenceResetAfterRegrowth(t *testing.T) {
	p, ctx, c := preflightPool(t)
	// Only the second reset must be cluster-wide (it moves the stats_reset
	// epoch); a cluster-wide reset wipes other packages' workloads.
	preflightResetDatabase(t, p)
	preflightExec(t, p, "SELECT sum(i) FROM generate_series(1,1000) i", 10)
	before := preflightCollect(t, c)
	q1 := preflightWorkload(t, before)
	c.latest = before
	c.recordQueryStore(ctx, before)
	var oldEpoch, newEpoch time.Time
	if err := p.QueryRow(ctx, "SELECT stats_reset FROM pg_stat_statements_info").Scan(
		&oldEpoch); err != nil {
		t.Fatal(err)
	}
	preflightExec(t, p, "/* pg_sage */ SELECT pg_stat_statements_reset()", 1)
	preflightExec(t, p, "SELECT sum(i) FROM generate_series(1,1000) i", 30)
	after := preflightCollect(t, c)
	q2 := preflightWorkload(t, after)
	c.recordQueryStore(ctx, after)
	if err := p.QueryRow(ctx, "SELECT stats_reset FROM pg_stat_statements_info").Scan(
		&newEpoch); err != nil {
		t.Fatal(err)
	}
	if !newEpoch.After(oldEpoch) || q2.Calls <= q1.Calls {
		t.Fatalf("invalid fixture: epochs=%v/%v calls=%d/%d", oldEpoch, newEpoch,
			q1.Calls, q2.Calls)
	}
	t.Logf("real reset %v -> %v; workload calls %d -> %d; StatsReset=%v",
		oldEpoch, newEpoch, q1.Calls, q2.Calls, after.StatsReset)
	if !after.StatsReset {
		t.Error("collector missed confirmed reset after counter regrowth")
	}
	ms, ok, err := querystore.WindowedLatencyMs(ctx, p, q1.QueryID,
		before.CollectedAt.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cross-epoch latency ms=%f ok=%v", ms, ok)
	if ok {
		t.Error("query store certified a window crossing confirmed statistics epochs")
	}
}

func preflightRoleRun(t *testing.T, p *pgxpool.Pool, role string, n int) {
	t.Helper()
	ctx := context.Background()
	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, "SET ROLE "+role); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, err = conn.Exec(ctx, "RESET ROLE")
		if err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < n; i++ {
		if _, err = conn.Exec(ctx, "SELECT sum(i) FROM generate_series(1,1000) i"); err != nil {
			t.Fatal(err)
		}
	}
}

func preflightTotals(s *Snapshot) (int64, int64, float64) {
	var id, calls int64
	var total float64
	for _, q := range s.Queries {
		if strings.Contains(q.Query, "sum(i)") {
			id = q.QueryID
			calls += q.Calls
			total += q.TotalExecTime
		}
	}
	return id, calls, total
}

// preflightRoles creates two roles unique to this process. Adapted setup:
// roles are cluster-wide, so fixed names collide with earlier runs and
// with other packages or agents sharing the fixture server.
func preflightRoles(t *testing.T, p *pgxpool.Pool) (string, string) {
	t.Helper()
	a := fmt.Sprintf("preflight_role_a_%d", os.Getpid())
	b := fmt.Sprintf("preflight_role_b_%d", os.Getpid())
	for _, r := range []string{a, b} {
		preflightExec(t, p, "DROP ROLE IF EXISTS "+r, 1)
		preflightExec(t, p, "CREATE ROLE "+r, 1)
		t.Cleanup(func() { preflightExec(t, p, "DROP ROLE IF EXISTS "+r, 1) })
	}
	return a, b
}

// preflightResetDatabase resets pg_stat_statements for the fixture
// database only. Adapted setup: a no-argument reset clears every database
// on the shared server, including other test packages' workloads.
func preflightResetDatabase(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	preflightExec(t, p, `/* pg_sage */ SELECT pg_stat_statements_reset(0,
		(SELECT oid FROM pg_database WHERE datname = current_database()), 0)`, 1)
}

func TestPreflightEvidenceRoleIdentityWindow(t *testing.T) {
	p, ctx, c := preflightPool(t)
	roleA, roleB := preflightRoles(t, p)
	preflightResetDatabase(t, p)
	preflightRoleRun(t, p, roleA, 10)
	preflightRoleRun(t, p, roleB, 100)
	before := preflightCollect(t, c)
	id, calls1, total1 := preflightTotals(before)
	c.recordQueryStore(ctx, before)
	preflightRoleRun(t, p, roleA, 10)
	preflightRoleRun(t, p, roleB, 100)
	after := preflightCollect(t, c)
	_, calls2, total2 := preflightTotals(after)
	c.recordQueryStore(ctx, after)
	var roles int
	if err := p.QueryRow(ctx, `SELECT count(DISTINCT userid) FROM pg_stat_statements
		WHERE queryid=$1 AND dbid=(SELECT oid FROM pg_database
		WHERE datname=current_database())`, id).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if roles != 2 || calls2-calls1 != 110 {
		t.Fatalf("invalid role fixture: roles=%d delta=%d", roles, calls2-calls1)
	}
	expected := (total2 - total1) / float64(calls2-calls1)
	ms, ok, err := querystore.WindowedLatencyMs(ctx, p, id,
		before.CollectedAt.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("roles=%d aggregate expected=%f; actual=%f ok=%v", roles, expected, ms, ok)
	if !ok || math.Abs(ms-expected) > 0.000001 {
		t.Errorf("same-query role cohorts lost: got %f/%v want aggregate %f", ms, ok, expected)
	}
}

func TestPreflightEvidenceTopLevelIdentity(t *testing.T) {
	// Adapted setup: nested statements are recorded only with
	// pg_stat_statements.track = 'all' (default 'top'); it is superuser-
	// settable per session, so every fixture connection sets it.
	p, ctx, c := preflightPoolWith(t, map[string]string{
		"pg_stat_statements.track": "all"})
	preflightExec(t, p, `CREATE OR REPLACE FUNCTION preflight_nested() RETURNS bigint
		LANGUAGE plpgsql AS $$ DECLARE v bigint; BEGIN
		SELECT sum(i) INTO v FROM generate_series(1,1000) i; RETURN v; END $$`, 1)
	preflightResetDatabase(t, p)
	preflightExec(t, p, "SELECT sum(i) FROM generate_series(1,1000) i", 10)
	preflightExec(t, p, "SELECT preflight_nested()", 100)
	before := preflightCollect(t, c)
	id, calls1, total1 := preflightTotals(before)
	c.recordQueryStore(ctx, before)
	preflightExec(t, p, "SELECT sum(i) FROM generate_series(1,1000) i", 10)
	preflightExec(t, p, "SELECT preflight_nested()", 100)
	after := preflightCollect(t, c)
	_, calls2, total2 := preflightTotals(after)
	c.recordQueryStore(ctx, after)
	var levels int
	if err := p.QueryRow(ctx, `SELECT count(DISTINCT toplevel) FROM pg_stat_statements
		WHERE queryid=$1 AND dbid=(SELECT oid FROM pg_database
		WHERE datname=current_database())`, id).Scan(&levels); err != nil {
		t.Fatal(err)
	}
	if levels != 2 || calls2-calls1 != 110 {
		t.Fatalf("unable to establish same-query top-level fixture: levels=%d calls=%d",
			levels, calls2-calls1)
	}
	expected := (total2 - total1) / float64(calls2-calls1)
	ms, ok, err := querystore.WindowedLatencyMs(ctx, p, id,
		before.CollectedAt.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("levels=%d aggregate expected=%f; actual=%f ok=%v", levels, expected, ms, ok)
	if !ok || math.Abs(ms-expected) > 0.000001 {
		t.Errorf("top-level cohorts lost: got %f/%v want aggregate %f", ms, ok, expected)
	}
}
