package autonomy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// D5: retention deletes act only on the owner-declared column, and a
// reviewed dry run authorizes deletion only for the exact relation, column
// identity and contract version it was recorded against. Real PostgreSQL.
// No concurrent access tests: the enforcer runs one batch per schema-guard
// cycle and the in-transaction identity recheck is covered by
// TestRetentionDeleteRechecksColumnIdentityInTx.

func declaredRetentionTable(t *testing.T, pool *pgxpool.Pool, columns, rows string) string {
	t.Helper()
	ctx := context.Background()
	table := fmt.Sprintf("ret_declared_%d", time.Now().UnixNano())
	for _, statement := range []string{
		"CREATE TABLE " + table + " " + columns,
		"INSERT INTO " + table + " VALUES " + rows,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("declared retention fixture %q: %v", statement, err)
		}
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = pool.Exec(cleanup, "DELETE FROM sage.retention_run WHERE table_name=$1", table)
		_, _ = pool.Exec(cleanup, "DELETE FROM sage.table_contract WHERE table_name=$1", table)
		_, _ = pool.Exec(cleanup, "DROP TABLE IF EXISTS "+table+" CASCADE")
	})
	if _, err := policy.NewStore(pool).Bootstrap(
		ctx, policy.Scope{}, "unattended", "d5-retention",
	); err != nil {
		t.Fatalf("bootstrap retention consent: %v", err)
	}
	return table
}

// createdAtFixture has two rows older than the 30-day window and one fresh
// row; every source_ts is years old, so a swap onto it would delete all.
func createdAtFixture(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	table := declaredRetentionTable(t, pool,
		"(id bigint, created_at timestamptz NOT NULL, source_ts timestamptz NOT NULL)",
		"(1, now()-interval '60 days', now()-interval '900 days'),"+
			"(2, now()-interval '45 days', now()-interval '900 days'),"+
			"(3, now(), now()-interval '900 days')")
	declareRetentionContract(t, pool, table, "created_at")
	return table
}

func agedDryRun(t *testing.T, pool *pgxpool.Pool, enforcer *postgresRetentionEnforcer,
	item schemaguard.Remediation,
) {
	t.Helper()
	item.Decision.Disposition = schemaguard.DispositionDryRun
	if err := enforcer.Apply(context.Background(), item); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	ageRetentionDryRuns(t, pool, item.Invariant.Table, 48*time.Hour)
}

func requireNothingDeleted(t *testing.T, pool *pgxpool.Pool, table string, rows int64) {
	t.Helper()
	if n := countRows(t, pool, "SELECT count(*) FROM "+table); n != rows {
		t.Fatalf("rows = %d, want %d: retention deleted data", n, rows)
	}
	applied := countRows(t, pool, fmt.Sprintf(`SELECT count(*) FROM sage.retention_run
		WHERE table_name='%s' AND disposition='applied'`, table))
	if applied != 0 {
		t.Fatalf("applied retention runs = %d, want 0", applied)
	}
}

func detectedRetentionItem(t *testing.T, pool *pgxpool.Pool, table string,
) schemaguard.Remediation {
	t.Helper()
	ctx := context.Background()
	items, err := newPostgresSchemaDetector(pool, nil).detectUnboundedAppend(ctx)
	if err != nil {
		t.Fatalf("detectUnboundedAppend: %v", err)
	}
	for _, invariant := range items {
		if invariant.Table != table {
			continue
		}
		contracts, err := (postgresSchemaContractSource{pool}).Contracts(ctx,
			[]schemaguard.Invariant{invariant})
		if err != nil {
			t.Fatalf("read contract: %v", err)
		}
		contract := contracts[invariant.Target()]
		return schemaguard.Remediation{Invariant: invariant, Contract: contract,
			Decision: schemaguard.Decision{Route: schemaguard.RouteRetention}}
	}
	t.Fatalf("no unbounded-append invariant detected for %s", table)
	return schemaguard.Remediation{}
}

func TestRetentionUsesDeclaredColumnNotCreatedAt(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := declaredRetentionTable(t, pool,
		"(id bigint, created_at timestamptz NOT NULL, ingested_at timestamptz NOT NULL)",
		"(1, now()-interval '400 days', now()),"+
			"(2, now()-interval '400 days', now()-interval '1 day')")
	declareRetentionContract(t, pool, table, "ingested_at")
	item := detectedRetentionItem(t, pool, table)
	if item.Invariant.RetentionColumn != "ingested_at" {
		t.Errorf("detected retention column = %q, want declared ingested_at",
			item.Invariant.RetentionColumn)
	}
	enforcer := &postgresRetentionEnforcer{pool: pool, batchLimit: 10, pipeline: allowRetention}
	agedDryRun(t, pool, enforcer, item)
	item.Decision.Disposition = schemaguard.DispositionApply

	err := enforcer.Apply(context.Background(), item)

	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n := countRows(t, pool, "SELECT count(*) FROM "+table); n != 2 {
		t.Fatalf("rows = %d, want 2: fresh ingested_at rows must survive", n)
	}
}

func scanRetentionGuard(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	guard, err := NewPostgresSchemaGuard(pool, "testdb", &recordingRouter{},
		ledger.NewService(ledger.NewPostgresRepository(pool)), allowRetention,
		SchemaGuardOptions{})
	if err != nil {
		t.Fatalf("NewPostgresSchemaGuard: %v", err)
	}
	if _, err := guard.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
}

func latestSchemaDecision(t *testing.T, pool *pgxpool.Pool, table string) (string, string) {
	t.Helper()
	target, err := json.Marshal([]string{"public." + table})
	if err != nil {
		t.Fatalf("encode target: %v", err)
	}
	var verdict, reason string
	err = pool.QueryRow(context.Background(), `SELECT verdict, reason FROM sage.decision
		WHERE feature='schema_guard' AND intent=$1 AND target_objects @> $2::jsonb
		ORDER BY id DESC LIMIT 1`, string(schemaguard.InvariantUnboundedAppend), target).
		Scan(&verdict, &reason)
	if err != nil {
		t.Fatalf("read schema decision for %s: %v", table, err)
	}
	return verdict, reason
}

func TestLegacyContractWithoutColumnParksAndDeletesNothing(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := declaredRetentionTable(t, pool, "(id bigint, created_at timestamptz NOT NULL)",
		"(1, now()-interval '400 days'), (2, now()-interval '300 days')")
	declareRetentionContract(t, pool, table, "")

	scanRetentionGuard(t, pool)
	ageRetentionDryRuns(t, pool, table, 48*time.Hour)
	scanRetentionGuard(t, pool)

	verdict, reason := latestSchemaDecision(t, pool, table)
	if verdict != string(ledger.VerdictPark) {
		t.Fatalf("verdict = %q, want %q", verdict, ledger.VerdictPark)
	}
	for _, want := range []string{"not declared", "suggested: created_at", "retention.column"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("park reason %q lacks %q", reason, want)
		}
	}
	requireNothingDeleted(t, pool, table, 2)
	runs := countRows(t, pool, fmt.Sprintf(
		"SELECT count(*) FROM sage.retention_run WHERE table_name='%s'", table))
	if runs != 0 {
		t.Fatalf("retention runs = %d for an undeclared column, want 0", runs)
	}
}

func TestDeclaredNonTemporalRetentionColumnParks(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := declaredRetentionTable(t, pool,
		"(id bigint, note text, created_at timestamptz NOT NULL)",
		"(1, 'a', now()-interval '400 days')")
	declareRetentionContract(t, pool, table, "note")

	scanRetentionGuard(t, pool)

	verdict, reason := latestSchemaDecision(t, pool, table)
	if verdict != string(ledger.VerdictPark) || !strings.Contains(reason, `"note"`) ||
		!strings.Contains(reason, "suggested: created_at") {
		t.Fatalf("decision = %s %q, want park naming note and the suggestion", verdict, reason)
	}
	requireNothingDeleted(t, pool, table, 1)
}

// The detector must emit one invariant per table however many contract rows
// name it. Duplicate NULL database_id rows are now rejected on write
// (idx_table_contract_identity), so the second row uses another database_id,
// which the detector's per-table lookup also ignores.
func TestUnboundedAppendDedupesContractsPerTable(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := createdAtFixture(t, pool)
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.table_contract
		(database_id, schema_name, table_name, append_only, retention_interval,
		 retention_column, declared_by, evidence_id)
		VALUES (990301,'public',$1,true,interval '30 days','created_at','test',$2)`,
		table, "duplicate_"+table); err != nil {
		t.Fatalf("second contract for the table: %v", err)
	}
	items, err := newPostgresSchemaDetector(pool, nil).
		detectUnboundedAppend(context.Background())
	if err != nil {
		t.Fatalf("detectUnboundedAppend: %v", err)
	}
	matches := 0
	for _, item := range items {
		if item.Table == table {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("invariants for %s = %d, want 1", table, matches)
	}
}
