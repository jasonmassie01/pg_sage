package schema

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// config-04b on a real server: the ledger is seeded by the first boot, a
// later boot issues no DDL, a changed checksum fails the boot, and an
// INVALID index is detected and rebuilt.

// ledgerRows reads the steps of sage.schema_migrations as id -> checksum
// (the catalog fingerprint row left out).
func ledgerRows(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id, checksum, pg_sage_version,
		applied_at <= now() FROM sage.schema_migrations WHERE id <> $1`, ledgerCatalogID)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, sum, version string
		var past bool
		if err := rows.Scan(&id, &sum, &version, &past); err != nil {
			t.Fatalf("scan the ledger: %v", err)
		}
		if version == "" || !past {
			t.Fatalf("ledger row %s: version %q, applied_at in the past %v", id, version, past)
		}
		out[id] = sum
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	return out
}

// ddlStatementsSinceReset returns the DDL statements (CREATE, ALTER, DROP,
// COMMENT, GRANT, REVOKE, TRUNCATE, REINDEX, and every DO block that is not
// a data statement) pg_stat_statements counted in the pool's database
// since resetStatements.
func ddlStatementsSinceReset(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT query, calls FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`)
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	type stmt struct {
		query string
		calls int64
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (stmt, error) {
		var s stmt
		return s, r.Scan(&s.query, &s.calls)
	})
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	var ddl []string
	for _, s := range all {
		head := strings.ToUpper(strings.TrimSpace(stripSQLComments(s.query)))
		if strings.HasPrefix(head, "DO ") && !isDataStatement(s.query) {
			ddl = append(ddl, strings.Join(strings.Fields(s.query), " "))
			continue
		}
		for _, kw := range []string{"CREATE ", "ALTER ", "DROP ", "COMMENT ",
			"GRANT ", "REVOKE ", "TRUNCATE ", "REINDEX "} {
			if strings.HasPrefix(head, kw) {
				ddl = append(ddl, strings.Join(strings.Fields(s.query), " "))
				break
			}
		}
	}
	return ddl
}

func resetStatements(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		t.Skipf("pg_stat_statements unavailable (needs shared_preload_libraries): %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_stat_statements_reset(0,
		(SELECT oid FROM pg_database WHERE datname = current_database()), 0)`); err != nil {
		t.Fatalf("reset pg_stat_statements: %v", err)
	}
}

// assertBootRunsNoDDL boots pool once more and fails on any DDL it issued.
func assertBootRunsNoDDL(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	resetStatements(t, pool)
	if err := Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if ddl := ddlStatementsSinceReset(t, pool); len(ddl) > 0 {
		t.Fatalf("a boot over a recorded ledger ran %d DDL statements:\n%s",
			len(ddl), strings.Join(ddl, "\n"))
	}
}

// Happy path: a fresh install records every step with its checksum, and
// the ledger equals the golden file (the steps outside migrationSteps too).
func TestBootstrap_LedgerRecordsEveryStepOnAFreshInstall(t *testing.T) {
	pool := freshSchemaPool(t, "schema_ledger_fresh")
	got := ledgerRows(t, pool)
	for _, s := range migrationSteps() {
		if got[s.name] != stepChecksum(s.sql) {
			t.Errorf("step %s: ledger checksum %q, want %s", s.name, got[s.name],
				stepChecksum(s.sql))
		}
	}
	golden, order := goldenChecksums(t)
	for _, id := range order {
		if got[id] != golden[id] {
			t.Errorf("ledger %s = %q, golden %s", id, got[id], golden[id])
		}
	}
	for id, sum := range got {
		if _, ok := golden[id]; !ok {
			t.Errorf("ledger step %s is not pinned: add '%s %s' to "+
				"testdata/migration_checksums.txt", id, id, sum)
		}
	}
}

// CHECK-01: a boot over a recorded ledger issues zero DDL statements.
func TestBootstrap_SecondBootIssuesNoDDL(t *testing.T) {
	assertBootRunsNoDDL(t, freshSchemaPool(t, "schema_ledger_noddl"))
}

// Upgrade: a v2.4.0 schema boots to the fresh catalog and the fresh ledger
// (seeded from the catalog checks), then a second start runs no DDL.
func TestBootstrap_UpgradeFromV240SeedsTheLedger(t *testing.T) {
	fresh := freshSchemaPool(t, "schema_ledger_up_fresh")
	pool := newPool(t, "schema_ledger_up_v240")
	restoreDump(t, pool, v240SageDump)
	if err := Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("upgrade boot: %v", err)
	}
	if d := diffCatalogs(sageCatalog(t, fresh), sageCatalog(t, pool)); d != "" {
		t.Fatalf("v2.4.0 upgraded differs from a fresh install:%s", d)
	}
	want, got := ledgerRows(t, fresh), ledgerRows(t, pool)
	if len(got) != len(want) {
		t.Fatalf("upgraded ledger has %d steps, fresh %d", len(got), len(want))
	}
	for id, sum := range want {
		if got[id] != sum {
			t.Errorf("upgraded ledger %s = %q, fresh %s", id, got[id], sum)
		}
	}
	assertBootRunsNoDDL(t, pool)
}

// State transition: a changed checksum fails the boot loudly and changes
// nothing; deleting the step's row (the documented recovery) re-runs it
// through the catalog checks and records the current checksum.
func TestBootstrap_ChangedChecksumFailsLoudly(t *testing.T) {
	pool := freshSchemaPool(t, "schema_ledger_changed")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE sage.schema_migrations SET checksum = 'deadbeef'
		WHERE id = 'ddlUsersOAuth'`); err != nil {
		t.Fatalf("tamper with the ledger: %v", err)
	}
	before := sageCatalog(t, pool)
	err := Bootstrap(ctx, pool)
	if !errors.Is(err, errMigrationChanged) {
		t.Fatalf("boot over a changed step: err = %v, want errMigrationChanged", err)
	}
	for _, want := range []string{"ddlUsersOAuth", "deadbeef",
		"DELETE FROM sage.schema_migrations WHERE id = 'ddlUsersOAuth'"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if d := diffCatalogs(before, sageCatalog(t, pool)); d != "" {
		t.Fatalf("the failed boot changed the catalog:%s", d)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.schema_migrations
		WHERE id = 'ddlUsersOAuth'`); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("boot after the recovery: %v", err)
	}
	if got := ledgerRows(t, pool)["ddlUsersOAuth"]; got != stepChecksum(ddlUsersOAuth) {
		t.Fatalf("recovered ledger row = %q, want %s", got, stepChecksum(ddlUsersOAuth))
	}
}

// indexValid reports whether sage.name exists and is valid.
func indexValid(t *testing.T, pool *pgxpool.Pool, name string) (bool, bool) {
	t.Helper()
	var exists, valid bool
	err := pool.QueryRow(context.Background(), `SELECT count(*) > 0,
		COALESCE(bool_and(i.indisvalid), false) FROM pg_index i
		WHERE i.indexrelid = to_regclass('sage.' || $1)`, name).Scan(&exists, &valid)
	if err != nil {
		t.Fatalf("read index %s: %v", name, err)
	}
	return exists, valid
}

// Invalid input: an INVALID index (what a killed CREATE INDEX CONCURRENTLY
// leaves) under a recorded step is detected and rebuilt valid.
func TestBootstrap_InvalidIndexIsRebuilt(t *testing.T) {
	pool := freshSchemaPool(t, "schema_ledger_invalid")
	ctx := context.Background()
	const idx = "idx_findings_rule_id" // step ddlFindingsAbsorbsSchemaFindings
	if _, err := pool.Exec(ctx, `UPDATE pg_index SET indisvalid = false
		WHERE indexrelid = to_regclass('sage.' || $1)`, idx); err != nil {
		t.Skipf("cannot mark an index invalid (needs superuser): %v", err)
	}
	if exists, valid := indexValid(t, pool, idx); !exists || valid {
		t.Fatalf("setup: %s exists=%v valid=%v, want an invalid index", idx, exists, valid)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("boot over an invalid index: %v", err)
	}
	if exists, valid := indexValid(t, pool, idx); !exists || !valid {
		t.Fatalf("after the boot %s exists=%v valid=%v, want rebuilt valid", idx, exists, valid)
	}
	assertBootRunsNoDDL(t, pool)
}

// Concurrency: two first boots at once leave one ledger row per step.
func TestBootstrap_ParallelBootsRecordEachStepOnce(t *testing.T) {
	pool := newPool(t, "schema_ledger_parallel")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = Bootstrap(ctx, pool)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("parallel boots: %v", err)
	}
	var rows, ids int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT id)
		FROM sage.schema_migrations WHERE id <> $1`, ledgerCatalogID).
		Scan(&rows, &ids); err != nil {
		t.Fatalf("count the ledger: %v", err)
	}
	golden, _ := goldenChecksums(t)
	if rows != ids || ids != len(golden) {
		t.Fatalf("ledger has %d rows, %d ids, want %d once each", rows, ids, len(golden))
	}
	if !slices.Contains(migrationStatements(), ddlSchemaMigrations) {
		t.Fatal("the ledger table is not a migration step")
	}
}

// State transition: a column a recorded step adds, dropped since, is added
// back by the next boot (drift repair, as before the ledger).
func TestBootstrap_RecordedStepRepairsADroppedColumn(t *testing.T) {
	pool := freshSchemaPool(t, "schema_ledger_drift")
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		"ALTER TABLE sage.query_hints DROP COLUMN rewrite_rationale"); err != nil {
		t.Fatalf("drop the column: %v", err)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("boot after the drop: %v", err)
	}
	var present bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute
		WHERE attrelid = 'sage.query_hints'::regclass AND attname = 'rewrite_rationale'
		  AND NOT attisdropped)`).Scan(&present); err != nil || !present {
		t.Fatalf("rewrite_rationale present=%v (%v), want re-added", present, err)
	}
	if got := ledgerRows(t, pool)["ddlQueryHintsRewrite"]; got !=
		stepChecksum(ddlQueryHintsRewrite) {
		t.Fatalf("ledger row = %q after the repair", got)
	}
	assertBootRunsNoDDL(t, pool) // the repaired catalog is sealed again
}

// The fingerprint row holds the catalog a complete boot left, and a boot
// that ran nothing does not rewrite it.
func TestBootstrap_SealsTheCatalogFingerprint(t *testing.T) {
	pool := freshSchemaPool(t, "schema_ledger_seal")
	ctx := context.Background()
	read := func() (sealed, current, at string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT checksum, (`+catalogFingerprintSQL+`),
			applied_at::text FROM sage.schema_migrations WHERE id = $1`,
			ledgerCatalogID).Scan(&sealed, &current, &at); err != nil {
			t.Fatalf("read the fingerprint row: %v", err)
		}
		return sealed, current, at
	}
	sealed, current, at := read()
	if len(sealed) != 32 || sealed != current {
		t.Fatalf("sealed %q, current catalog %q: want the md5 of the current catalog",
			sealed, current)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if again, _, atAgain := read(); again != sealed || atAgain != at {
		t.Fatalf("a boot that ran nothing rewrote the fingerprint (%s at %s -> %s at %s)",
			sealed, at, again, atAgain)
	}
}
