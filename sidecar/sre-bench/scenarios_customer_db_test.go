package srebench

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/gameday"
)

// benches-12 integration: the customer program on a clone holding a
// 1M-row customer table. It manifests at least three lock waiters, is
// graded, recovers (no bench session, no waiter left) and leaves the
// table's rows and write counters as they were. The schema and table
// names carry a dot and a quote, so only the quoted identifier works.

const (
	customerSchema = "Sales.EU"
	customerTable  = `order"lines`
)

func customerBinding() gameday.Binding {
	return gameday.Binding{Program: gameday.ProgramCustomerHotTable, Schema: customerSchema,
		Table: customerTable}
}

// customerFixture creates the customer table with rows rows and drops it
// when the test ends.
func customerFixture(ctx context.Context, t *testing.T, e *Env, rows int) string {
	t.Helper()
	ident := customerBinding().Ident()
	schema := pgx.Identifier{customerSchema}.Sanitize()
	if _, err := e.Pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.Pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	if _, err := e.Pool.Exec(ctx, "CREATE SCHEMA "+schema+"; CREATE TABLE "+ident+
		" (id bigint PRIMARY KEY, v int NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pool.Exec(ctx, "INSERT INTO "+ident+
		" SELECT g, 0 FROM generate_series(1, $1::int) g", rows); err != nil {
		t.Fatal(err)
	}
	return ident
}

type tableCounters struct{ rows, ins, upd, del int64 }

func countersOf(ctx context.Context, t *testing.T, e *Env, ident string) tableCounters {
	t.Helper()
	if _, err := e.Pool.Exec(ctx, "SELECT pg_stat_clear_snapshot()"); err != nil {
		t.Fatal(err)
	}
	var c tableCounters
	if err := e.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+ident+`),
		n_tup_ins, n_tup_upd, n_tup_del FROM pg_catalog.pg_stat_user_tables
		WHERE relid = to_regclass($1)`, ident).Scan(&c.rows, &c.ins, &c.upd,
		&c.del); err != nil {
		t.Fatalf("read the counters of %s: %v", ident, err)
	}
	return c
}

// observingArm counts the lock waiters when the investigation starts,
// then investigates with the causal graph.
type observingArm struct {
	CausalGraph
	waiters int
}

func (a *observingArm) Investigate(ctx context.Context, e *Env, sc Scenario) (Trace, error) {
	n, err := e.lockWaiters(ctx)
	if err != nil {
		return Trace{}, err
	}
	a.waiters = n
	return a.CausalGraph.Investigate(ctx, e, sc)
}

func customerScenario(t *testing.T, b gameday.Binding) Scenario {
	t.Helper()
	scs, err := customerScenarios([]gameday.Binding{b})
	if err != nil || len(scs) != 1 {
		t.Fatalf("customer scenario: %v", err)
	}
	return scs[0]
}

func TestCustomerHotTableOnAMillionRowTable(t *testing.T) {
	_, e := liveEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	ident := customerFixture(ctx, t, e, 1_000_000)
	before := countersOf(ctx, t, e, ident)
	if before.rows != 1_000_000 {
		t.Fatalf("the customer table has %d rows, want 1000000", before.rows)
	}
	arm := &observingArm{}
	sc := customerScenario(t, customerBinding())
	start := time.Now()
	r := e.attempt(ctx, sc, arm)
	if r.Err != nil || r.Skipped != "" {
		t.Fatalf("customer program: err %v, skipped %q", r.Err, r.Skipped)
	}
	if arm.waiters < 3 {
		t.Fatalf("%d lock waiters at the investigation, want at least 3", arm.waiters)
	}
	g := GradeResult(r)
	if !g.Top1 || !g.SafePass || len(r.Outcome.Forbidden) != 0 || r.Outcome.ProbeCount == 0 {
		t.Fatalf("grade = %+v, outcome root %q forbidden %v; want the queued lock found",
			g, r.Outcome.Root, r.Outcome.Forbidden)
	}
	if n, err := e.lockWaiters(ctx); err != nil || n != 0 {
		t.Fatalf("%d lock waiters after recovery (%v)", n, err)
	}
	if n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
		WHERE datname = current_database() AND application_name LIKE 'bench\_%'`); err != nil ||
		n != 0 {
		t.Fatalf("%d bench sessions after recovery (%v)", n, err)
	}
	after := countersOf(ctx, t, e, ident)
	if after != before {
		t.Fatalf("the program changed the table: before %+v, after %+v", before, after)
	}
	t.Logf("customer program on 1M rows took %s", time.Since(start).Round(time.Second))
}

// Invalid input: a binding to a table the clone lacks is skipped with
// "relation not in clone", and nothing is locked.
func TestCustomerBindingNotInCloneIsSkipped(t *testing.T) {
	ctx, e := liveEnv(t)
	b := gameday.Binding{Program: gameday.ProgramCustomerHotTable, Schema: "public",
		Table: "bench_no_such_customer_table"}
	r := e.attempt(ctx, customerScenario(t, b), CausalGraph{})
	if r.Skipped != skipNotInClone || r.Err != nil {
		t.Fatalf("skipped %q err %v, want %q", r.Skipped, r.Err, skipNotInClone)
	}
}

// Invalid input: the injection-shaped name is one quoted identifier; the
// program locks that table and the table its name mentions survives.
func TestCustomerHostileNameIsQuoted(t *testing.T) {
	ctx, e := liveEnv(t)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if _, err := e.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS bench_victim (id int)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.Pool.Exec(context.Background(), `DROP TABLE IF EXISTS bench_victim,
			"bench_x""; DROP TABLE bench_victim; --"`)
	})
	b := gameday.Binding{Program: gameday.ProgramCustomerHotTable, Schema: "public",
		Table: `bench_x"; DROP TABLE bench_victim; --`}
	if _, err := e.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+b.Ident()+
		" (id int)"); err != nil {
		t.Fatal(err)
	}
	r := e.attempt(ctx, customerScenario(t, b), &observingArm{})
	if r.Err != nil || r.Skipped != "" {
		t.Fatalf("hostile-named table: err %v, skipped %q", r.Err, r.Skipped)
	}
	if n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_class
		WHERE oid = to_regclass('public.bench_victim')`); err != nil || n != 1 {
		t.Fatalf("bench_victim count %d (%v): the name was not quoted", n, err)
	}
}

// Error propagation: the holder's LOCK TABLE refused with 42501 (a role
// that may only read the table) fails the run with the SQLSTATE and the
// binding hash, never the table name.
func TestCustomerHolderPermissionDeniedCarriesTheSQLSTATE(t *testing.T) {
	ctx, e := liveEnv(t)
	ident := customerFixture(ctx, t, e, 10)
	role := "bench_customer_reader"
	setup := `DROP ROLE IF EXISTS ` + role + `; CREATE ROLE ` + role +
		` LOGIN PASSWORD 'bench-reader'; GRANT USAGE ON SCHEMA ` +
		pgx.Identifier{customerSchema}.Sanitize() + ` TO ` + role +
		`; GRANT SELECT ON ` + ident + ` TO ` + role
	if err := e.simple(ctx, setup); err != nil {
		t.Fatalf("create the read-only role: %v", err)
	}
	t.Cleanup(func() {
		_ = e.simple(context.Background(), "DROP OWNED BY "+role+"; DROP ROLE IF EXISTS "+role)
	})
	u, err := url.Parse(e.DSN)
	if err != nil || u.Scheme == "" {
		t.Skipf("the test DSN is not a URL (%v); cannot derive the reader's DSN", err)
	}
	u.User = url.UserPassword(role, "bench-reader")
	owner := e.DSN
	e.DSN = u.String()
	defer func() { e.DSN = owner }()
	r := e.attempt(ctx, customerScenario(t, customerBinding()), CausalGraph{})
	if r.Err == nil || !strings.Contains(r.Err.Error(), "SQLSTATE 42501") {
		t.Fatalf("err = %v, want SQLSTATE 42501", r.Err)
	}
	if msg := r.Err.Error(); strings.Contains(msg, "lines") ||
		!strings.Contains(msg, customerBinding().Hash()) {
		t.Fatalf("err = %q, want the hash and no table name", msg)
	}
}

// Integration through the game-day adapter: the binding reaches the
// program, the customer run scores, the ledger reads the report, and the
// report (what is ingested and exported) never names the table.
func TestGameDayAdapterRunsTheCustomerProgram(t *testing.T) {
	ctx, e := liveEnv(t)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	customerFixture(ctx, t, e, 1000)
	f := GameDayFaults{Scenarios: []string{gameday.ProgramCustomerHotTable}}
	raw, err := f.Run(ctx, e.DSN, []string{"lock_blocking"},
		[]gameday.Binding{customerBinding()})
	if err != nil {
		t.Fatalf("game day: %v", err)
	}
	var report Report
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Runs) != 1 || report.Runs[0].Scenario != gameday.ProgramCustomerHotTable ||
		report.Runs[0].Skipped != "" || report.Runs[0].Error != "" ||
		report.Runs[0].Root != "ddl_lock_queue" {
		t.Fatalf("runs = %+v, want one scored customer run", report.Runs)
	}
	if _, err := earned.ParseBenchReport(raw, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("the ledger cannot read the report: %v", err)
	}
	for _, name := range []string{customerSchema, `order\"lines`, customerTable} {
		if strings.Contains(string(raw), name) {
			t.Fatalf("the report carries %q", name)
		}
	}
}

// Invalid input: a binding whose name is a view on the clone (not the
// ordinary table it was on the monitored database) is skipped, unlocked.
func TestCustomerBindingToAViewIsSkipped(t *testing.T) {
	ctx, e := liveEnv(t)
	if _, err := e.Pool.Exec(ctx,
		`CREATE OR REPLACE VIEW bench_customer_view AS SELECT 1 AS one`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.Pool.Exec(context.Background(), `DROP VIEW IF EXISTS bench_customer_view`)
	})
	b := gameday.Binding{Program: gameday.ProgramCustomerHotTable, Schema: "public",
		Table: "bench_customer_view"}
	r := e.attempt(ctx, customerScenario(t, b), CausalGraph{})
	if !strings.Contains(r.Skipped, "not an ordinary table") || r.Err != nil {
		t.Fatalf("skipped %q err %v, want the not-a-table skip", r.Skipped, r.Err)
	}
}
