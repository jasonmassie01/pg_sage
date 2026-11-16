//go:build integration

package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Integration (lab-04b): the rehearsal measures the statements that
// reference the table on the clone before and after the expand steps.
// The test database stands in for the clone (and, for the workload read,
// for production); the identity guard is given another production. Built
// without cgo the analyze guard cannot prove a statement read-only, so the
// statements are planned and judged on cost and plan hash; in the cgo
// container they are executed and judged on latency.

var notProduction = func(context.Context) (ServerIdentity, error) {
	return ServerIdentity{Host: "production.invalid", Port: 5432,
		StartedAt: time.Unix(1, 0), ServerPort: 5432}, nil
}

type liveClone struct{ dsn string }

func (c liveClone) SnapshotAge(context.Context) (time.Duration, error) { return 0, nil }
func (c liveClone) Create(context.Context, clonepkg.CloneSpec) (clonepkg.Clone, error) {
	return clonepkg.Clone{ID: "it-clone", DSN: c.dsn}, nil
}
func (liveClone) Destroy(context.Context, clonepkg.Clone) error { return nil }

// lookupTable creates table (id, email) with rows rows and an index on
// email, analyzed.
func lookupTable(t *testing.T, pool *pgxpool.Pool, name string, rows int) planpkg.TableFacts {
	t.Helper()
	ctx := context.Background()
	q := "public." + name
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+q); err != nil {
		t.Fatalf("drop fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE TABLE "+q+
		" (id bigint PRIMARY KEY, email text)"); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO "+q+
		" SELECT g, 'u' || g FROM generate_series(1, $1::int) g", rows); err != nil {
		t.Fatalf("fill fixture: %v", err)
	}
	for _, sql := range []string{"CREATE INDEX " + name + "_email_idx ON " + q + " (email)",
		"ANALYZE " + q} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+q) })
	return planpkg.TableFacts{Schema: "public", Name: name}
}

func rehearseOn(t *testing.T, dsn string, source WorkloadSource,
	planned planpkg.Plan) rehearsalpkg.Result {
	t.Helper()
	runner := NewPostgresRehearsalRunner(time.Second, time.Minute).
		WithProduction(notProduction).WithWorkload(source, 5*time.Second)
	result, err := rehearsalpkg.NewOrchestrator(liveClone{dsn}, runner, rehearsalpkg.Options{
		MaxCloneAge: time.Hour, RegressionPct: 15}).Rehearse(context.Background(), planned)
	if err != nil {
		t.Fatalf("Rehearse: %v", err)
	}
	return result
}

// dropIndexPlan is an expand step that loses the lookup index, so the
// statement's Index Scan becomes a Seq Scan.
func dropIndexPlan(table planpkg.TableFacts) planpkg.Plan {
	return planpkg.Plan{Table: table, ExpandSteps: []planpkg.Step{{
		Kind: planpkg.StepKind("drop_index"), Phase: planpkg.PhaseExpand,
		RequiresTopLevel: true, LockLevel: "SHARE UPDATE EXCLUSIVE",
		SQL: "DROP INDEX CONCURRENTLY public." + table.Name + "_email_idx"}}}
}

func TestRehearsalParksAnIndexToSeqScanFlip(t *testing.T) {
	pool := migrationRuntimePool(t)
	table := lookupTable(t, pool, "lab04b_flip", 200000)
	source := sourceOf([]AffectedStatement{{QueryID: 41,
		Text: "SELECT id FROM public.lab04b_flip WHERE email = 'u777'"}}, nil)
	got := rehearseOn(t, testdb.SkipUnlessLive(t), source, dropIndexPlan(table))
	if got.Verdict != rehearsalpkg.VerdictPark ||
		got.Reason != rehearsalpkg.ReasonPlanRegression ||
		!strings.Contains(got.Detail, "queryid 41") ||
		!strings.Contains(got.Detail, "plan changed") {
		t.Fatalf("result = %#v", got)
	}
	q := got.Measurement.AffectedQueries[0]
	if q.BeforePlanHash == "" || q.BeforePlanHash == q.AfterPlanHash || q.Unmeasured != "" ||
		q.AfterCost <= q.BeforeCost {
		t.Fatalf("query = %#v", q)
	}
}

// A harmless expand (a NOT VALID check, then its validation) promotes; a
// statement text holding two statements is skipped with the reason.
func TestRehearsalPromotesAHarmlessExpand(t *testing.T) {
	pool := migrationRuntimePool(t)
	table := lookupTable(t, pool, "lab04b_ok", 20000)
	source := sourceOf([]AffectedStatement{
		{QueryID: 51, Text: "SELECT id FROM public.lab04b_ok WHERE email = 'u42'"},
		{QueryID: 52, Text: "SELECT 1 FROM public.lab04b_ok; SELECT 2"},
	}, nil)
	planned := planpkg.Plan{Table: table, ExpandSteps: []planpkg.Step{
		{Kind: planpkg.StepAddCheckNotValid, Phase: planpkg.PhaseExpand,
			LockLevel: "ACCESS EXCLUSIVE", SQL: "ALTER TABLE public.lab04b_ok ADD " +
				"CONSTRAINT lab04b_ok_nn CHECK (email IS NOT NULL) NOT VALID"},
		{Kind: planpkg.StepValidateConstraint, Phase: planpkg.PhaseExpand,
			LockLevel: "SHARE UPDATE EXCLUSIVE",
			SQL:       "ALTER TABLE public.lab04b_ok VALIDATE CONSTRAINT lab04b_ok_nn"},
	}}
	got := rehearseOn(t, testdb.SkipUnlessLive(t), source, planned)
	if got.Verdict != rehearsalpkg.VerdictPromoteExpand || len(got.Measurement.Steps) != 2 {
		t.Fatalf("result = %#v", got)
	}
	queries := got.Measurement.AffectedQueries
	if len(queries) != 2 || queries[0].BeforePlanHash != queries[0].AfterPlanHash ||
		queries[0].Unmeasured != "" ||
		queries[1].Unmeasured != rehearsalpkg.UnmeasuredNotOne {
		t.Fatalf("queries = %#v", queries)
	}
}

// Error propagation: the identity guard refuses before the first
// statement; the production workload is never read.
func TestRehearsalIdentityRefusalMeasuresNothing(t *testing.T) {
	pool := migrationRuntimePool(t)
	reads := 0
	source := func(context.Context, planpkg.TableFacts) ([]AffectedStatement, error) {
		reads++
		return nil, nil
	}
	runner := NewPostgresRehearsalRunner(time.Second, time.Minute).
		WithProduction(PoolIdentity(pool)).WithWorkload(source, 0)
	_, err := runner.Run(context.Background(), testdb.SkipUnlessLive(t),
		planpkg.Plan{Table: planpkg.TableFacts{Schema: "public", Name: "users"}})
	if !errors.Is(err, rehearsalpkg.ErrTargetIsProduction) || reads != 0 {
		t.Fatalf("err = %v, workload reads = %d", err, reads)
	}
}

// The production workload comes from pg_stat_statements: statements that
// reference the table, and none that do not. On PostgreSQL 16+ their
// normalized ($n) texts are generic-planned and the flip parks; before
// 16 they cannot be planned and the rehearsal stays inconclusive.
func TestRehearsalMeasuresTheProductionWorkload(t *testing.T) {
	pool := migrationRuntimePool(t)
	table := lookupTable(t, pool, "lab04b_pgss", 200000)
	other := lookupTable(t, pool, "lab04b_other", 10)
	ctx := context.Background()
	var id int64
	for range 3 {
		_ = pool.QueryRow(ctx, "SELECT id FROM public.lab04b_pgss WHERE email = $1",
			"u9").Scan(&id)
		_ = pool.QueryRow(ctx, "SELECT id FROM public.lab04b_other WHERE email = $1",
			"u9").Scan(&id)
	}
	stmts, err := PostgresWorkload(pool)(ctx, table)
	if err != nil {
		t.Fatalf("PostgresWorkload: %v", err)
	}
	found := false
	for _, s := range stmts {
		if strings.Contains(s.Text, other.Name) || !strings.Contains(s.Text, table.Name) {
			t.Fatalf("statement %q does not reference only %s", s.Text, table.Name)
		}
		found = found || strings.Contains(s.Text, "WHERE email = $1")
	}
	if !found || len(stmts) > MaxAffectedStatements {
		t.Fatalf("statements = %#v", stmts)
	}
	got := rehearseOn(t, testdb.SkipUnlessLive(t), PostgresWorkload(pool),
		dropIndexPlan(table))
	if serverVersion(t, pool) >= 160000 {
		if got.Verdict != rehearsalpkg.VerdictPark {
			t.Fatalf("PG16+: result = %#v", got)
		}
		return
	}
	if got.Verdict != rehearsalpkg.VerdictRecommendOnly ||
		!strings.Contains(got.Detail, "PostgreSQL 16") {
		t.Fatalf("PG14/15: result = %#v", got)
	}
}

func serverVersion(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var v int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&v); err != nil {
		t.Fatalf("server version: %v", err)
	}
	return v
}

// Nil/empty: a database without pg_stat_statements has no statement texts,
// which reads differently from no statement referencing the table.
func TestPostgresWorkloadWithoutPgStatStatements(t *testing.T) {
	admin := migrationRuntimePool(t)
	ctx := context.Background()
	const name = "lab04b_nopgss"
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name) })
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	_, err = PostgresWorkload(pool)(ctx, planpkg.TableFacts{Schema: "public", Name: "t"})
	if !errors.Is(err, ErrNoStatementTexts) ||
		!strings.HasPrefix(err.Error(), "pg_stat_statements unavailable on production") {
		t.Fatalf("err = %v", err)
	}
	stmts, err := PostgresWorkload(admin)(ctx, planpkg.TableFacts{Schema: "public",
		Name: "lab04b_never_queried"})
	if err != nil || len(stmts) != 0 {
		t.Fatalf("unreferenced table: %#v, %v", stmts, err)
	}
}

// The recorder stores the ladder and the clone receipt with the
// measured statements.
func TestPostgresRecorderPersistsLadderAndReceipt(t *testing.T) {
	pool := migrationRuntimePool(t)
	rehearsal := rehearsalpkg.Result{Verdict: rehearsalpkg.VerdictPromoteExpand,
		Reason: rehearsalpkg.ReasonRehearsalPassed, CloneID: "c", ReceiptID: 55,
		At: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC),
		Measurement: rehearsalpkg.Measurement{AffectedQueries: []rehearsalpkg.QueryMeasurement{
			{QueryID: 9, BeforeCost: 8, AfterCost: 8, BeforePlanHash: "a", AfterPlanHash: "a",
				NotExecuted: "not executed: parameters unknown"},
			{QueryID: 10, Unmeasured: rehearsalpkg.UnmeasuredNotOne}}}}
	ladder := rehearsalpkg.Ladder(rehearsal)
	record := Record{Request: request(), Verdict: VerdictExpanded,
		EvidenceID: "ev_lab04b_ladder", Reason: ReasonContractPending,
		Measurement: &rehearsal.Measurement, Ladder: &ladder, CloneReceipt: 55}
	if err := NewPostgresRecorder(pool).Record(context.Background(), record); err != nil {
		t.Fatalf("Record: %v", err)
	}
	var cloneID, result, unmeasured string
	var receipt int64
	err := pool.QueryRow(context.Background(), `SELECT clone_id,
		measurement->'ladder'->>'result',
		(measurement->'ladder'->'evidence'->>'receipt_id')::bigint,
		measurement->'affected_queries'->1->>'unmeasured'
		FROM sage.migration_run WHERE evidence_id = $1`, record.EvidenceID).
		Scan(&cloneID, &result, &receipt, &unmeasured)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if cloneID != "55" || result != "passed" || receipt != 55 ||
		unmeasured != rehearsalpkg.UnmeasuredNotOne {
		t.Fatalf("clone_id=%q ladder=%q receipt=%d unmeasured=%q", cloneID, result, receipt,
			unmeasured)
	}
}
