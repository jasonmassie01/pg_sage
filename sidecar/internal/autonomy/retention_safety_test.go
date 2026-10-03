package autonomy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// Regression tests for G4-B04, Codex R01/R02/R03 and G2-B12: retention must
// delete only eligible rows (tableoid + ctid + cutoff), route through the
// policy gate, run with timeouts, and bind dry runs to column/window/relation.

// allowRetention stands in for the executor's pipeline: it authorizes and
// runs the batch at once, with the batch's own defaults.
func allowRetention(ctx context.Context, _ RetentionIntent, batch RetentionBatch) error {
	_, err := batch(ctx, RetentionRun{})
	return err
}

func retentionFixture(t *testing.T, pool *pgxpool.Pool, partitioned bool) string {
	t.Helper()
	ctx := context.Background()
	table := fmt.Sprintf("ret_safety_%d", time.Now().UnixNano())
	var statements []string
	if partitioned {
		statements = []string{
			"CREATE TABLE " + table + " (id bigint, created_at timestamptz NOT NULL)" +
				" PARTITION BY RANGE (created_at)",
			"CREATE TABLE " + table + "_old PARTITION OF " + table +
				" FOR VALUES FROM ('2000-01-01') TO ('2020-01-01')",
			"CREATE TABLE " + table + "_new PARTITION OF " + table +
				" FOR VALUES FROM ('2020-01-01') TO ('2200-01-01')",
			"INSERT INTO " + table + " VALUES (1, '2010-01-01'), (2, now())",
		}
	} else {
		statements = []string{
			"CREATE TABLE " + table + " (id bigint, created_at timestamptz NOT NULL)",
			"INSERT INTO " + table + " VALUES (1, now() - interval '60 days')," +
				" (2, now() - interval '45 days'), (3, now())",
		}
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("retention fixture %q: %v", statement, err)
		}
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = pool.Exec(cleanup, "DELETE FROM sage.retention_run WHERE table_name=$1", table)
		_, _ = pool.Exec(cleanup, "DELETE FROM sage.table_contract WHERE table_name=$1", table)
		_, _ = pool.Exec(cleanup, "DROP TABLE IF EXISTS "+table+" CASCADE")
	})
	declareRetentionContract(t, pool, table, "created_at")
	return table
}

// declareRetentionContract records an owner-declared 30-day retention
// contract; an empty column stores NULL, as a pre-D5 contract does.
func declareRetentionContract(t *testing.T, pool *pgxpool.Pool, table, column string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO sage.table_contract
		(schema_name, table_name, append_only, retention_interval, retention_column,
		 declared_by, evidence_id)
		VALUES ('public',$1,true,interval '30 days',NULLIF($2,''),'test',$3)`,
		table, column, "contract_"+table)
	if err != nil {
		t.Fatalf("declare retention contract for %s: %v", table, err)
	}
}

func retentionItem(table string, window time.Duration, disposition schemaguard.Disposition,
) schemaguard.Remediation {
	return schemaguard.Remediation{
		Invariant: schemaguard.Invariant{
			Kind:   schemaguard.InvariantUnboundedAppend,
			Schema: "public", Table: table, RetentionColumn: "created_at",
		},
		Contract: schemaguard.TableContract{AppendOnly: true, RetentionWindow: window,
			RetentionColumn: "created_at"},
		Decision: schemaguard.Decision{
			Route: schemaguard.RouteRetention, Disposition: disposition,
		},
	}
}

func mustRetentionTarget(t *testing.T, pool *pgxpool.Pool, invariant schemaguard.Invariant,
) retentionTarget {
	t.Helper()
	target, err := resolveRetentionTarget(context.Background(), pool, invariant)
	if err != nil {
		t.Fatalf("resolve retention target: %v", err)
	}
	return target
}

func ageRetentionDryRuns(t *testing.T, pool *pgxpool.Pool, table string, age time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE sage.retention_run
		SET created_at = created_at - make_interval(secs => $2),
		    cutoff_at = cutoff_at - make_interval(secs => $2)
		WHERE table_name=$1 AND disposition='dry_run'`, table, age.Seconds())
	if err != nil {
		t.Fatalf("age dry runs: %v", err)
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(context.Background(), query).Scan(&count); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return count
}

func TestRetentionDeleteNeverTouchesOtherPartitions(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := retentionFixture(t, pool, true)
	enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 1, pipeline: allowRetention}
	item := retentionItem(table, 30*24*time.Hour, schemaguard.DispositionApply)
	cutoff := time.Now().Add(-30 * 24 * time.Hour)

	target := mustRetentionTarget(t, pool, item.Invariant)
	result, err := enforcer.deleteBatch(context.Background(), retentionPlan{item: item,
		target: target, cutoff: cutoff, candidates: 1, bound: 10}, RetentionRun{})
	deleted := result.Deleted

	if err != nil {
		t.Fatalf("deleteBatch: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want exactly the batch limit 1", deleted)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM "+table+" WHERE id = 2"); n != 1 {
		t.Fatalf("future row in another partition was deleted")
	}
}

func TestRetentionDeleteStatementBindsIdentityAndCutoff(t *testing.T) {
	query := retentionDeleteSQL(`"public"."events"`, `"created_at"`)
	for _, want := range []string{
		"tableoid", "target.tableoid = victims.tableoid",
		"target.ctid = victims.ctid", `target."created_at" < $1`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("retention delete SQL lacks %q:\n%s", want, query)
		}
	}
}

func TestRetentionApplyRequiresAuthorization(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := retentionFixture(t, pool, false)
	ctx := context.Background()
	withheld := errors.New("emergency stop active")
	for _, pipeline := range []RetentionPipeline{
		nil, func(context.Context, RetentionIntent, RetentionBatch) error { return withheld },
	} {
		enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 10, pipeline: pipeline}
		dryRun := retentionItem(table, 30*24*time.Hour, schemaguard.DispositionDryRun)
		if err := enforcer.Apply(ctx, dryRun); err != nil {
			t.Fatalf("dry run: %v", err)
		}
		ageRetentionDryRuns(t, pool, table, 48*time.Hour)

		err := enforcer.Apply(ctx, retentionItem(table, 30*24*time.Hour,
			schemaguard.DispositionApply))

		if err == nil {
			t.Fatal("retention delete ran without an execute authorization")
		}
		if n := countRows(t, pool, "SELECT count(*) FROM "+table); n != 3 {
			t.Fatalf("rows = %d after withheld retention, want 3", n)
		}
	}
}

func TestRetentionApplyPassesIntentToAuthorizer(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := retentionFixture(t, pool, false)
	ctx := context.Background()
	var seen RetentionIntent
	enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 10,
		pipeline: func(ctx context.Context, intent RetentionIntent, batch RetentionBatch) error {
			seen = intent
			_, err := batch(ctx, RetentionRun{})
			return err
		}}
	window := 30 * 24 * time.Hour
	if err := enforcer.Apply(ctx, retentionItem(table, window,
		schemaguard.DispositionDryRun)); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	ageRetentionDryRuns(t, pool, table, 48*time.Hour)

	if err := enforcer.Apply(ctx, retentionItem(table, window,
		schemaguard.DispositionApply)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if seen.Table != table || seen.Column != "created_at" || seen.Window != window ||
		seen.BatchLimit != 10 {
		t.Fatalf("authorized intent = %#v", seen)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM "+table); n != 1 {
		t.Fatalf("rows = %d after authorized retention, want 1", n)
	}
}

func TestRetentionDryRunMustMatchCurrentSemantics(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := retentionFixture(t, pool, false)
	ctx := context.Background()
	enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 10, pipeline: allowRetention}
	if err := enforcer.Apply(ctx, retentionItem(table, 90*24*time.Hour,
		schemaguard.DispositionDryRun)); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	ageRetentionDryRuns(t, pool, table, 48*time.Hour)

	err := enforcer.Apply(ctx, retentionItem(table, 30*24*time.Hour,
		schemaguard.DispositionApply))

	if !errors.Is(err, ErrRetentionDryRunPending) {
		t.Fatalf("apply with changed window = %v, want ErrRetentionDryRunPending", err)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM "+table); n != 3 {
		t.Fatalf("rows = %d, window change must not delete", n)
	}
	fresh := countRows(t, pool, fmt.Sprintf(`SELECT count(*) FROM sage.retention_run
		WHERE table_name='%s' AND disposition='dry_run'
		AND created_at > now() - interval '1 hour'`, table))
	if fresh != 1 {
		t.Fatalf("fresh dry runs for new semantics = %d, want 1", fresh)
	}
}

func TestRetentionDryRunNeedsReviewWindow(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := retentionFixture(t, pool, false)
	ctx := context.Background()
	enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 10, pipeline: allowRetention}
	window := 30 * 24 * time.Hour
	if err := enforcer.Apply(ctx, retentionItem(table, window,
		schemaguard.DispositionDryRun)); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	err := enforcer.Apply(ctx, retentionItem(table, window, schemaguard.DispositionApply))

	if !errors.Is(err, ErrRetentionDryRunPending) {
		t.Fatalf("apply right after dry run = %v, want ErrRetentionDryRunPending", err)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM "+table); n != 3 {
		t.Fatalf("rows = %d, fresh dry run must not permit deletion", n)
	}
}

func TestRetentionDeleteUsesLockTimeout(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := retentionFixture(t, pool, false)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 10, pipeline: allowRetention}
	item := retentionItem(table, 30*24*time.Hour, schemaguard.DispositionApply)
	target := mustRetentionTarget(t, pool, item.Invariant)
	started := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, err = enforcer.deleteBatch(callCtx, retentionPlan{item: item, target: target,
		cutoff: time.Now().Add(-24 * time.Hour), bound: 10}, RetentionRun{})

	if err == nil || time.Since(started) > 15*time.Second {
		t.Fatalf("deleteBatch under lock: err=%v after %s, want bounded lock failure",
			err, time.Since(started))
	}
}

func TestUnboundedAppendDoesNotGuessArbitraryTimeColumn(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	table := fmt.Sprintf("ret_guess_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE TABLE "+table+
		" (id bigint, birth_date date, updated_at timestamptz)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.table_contract
		(schema_name, table_name, append_only, retention_interval, declared_by, evidence_id)
		VALUES ('public',$1,true,interval '30 days','test',$2)`,
		table, "contract_"+table); err != nil {
		t.Fatalf("contract: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.table_contract WHERE table_name=$1", table)
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})

	items, err := newPostgresSchemaDetector(pool, nil).detectUnboundedAppend(ctx)

	if err != nil {
		t.Fatalf("detectUnboundedAppend: %v", err)
	}
	for _, item := range items {
		if item.Table == table && item.RetentionColumn != "" {
			t.Fatalf("retention column guessed as %q", item.RetentionColumn)
		}
	}
}
