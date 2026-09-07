package schema

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

var agentNativeTableColumns = map[string][]string{
	"policy": {
		"id", "database_id", "version", "scope", "profile", "doc",
		"schema_version", "status", "proposed_by", "ratified_by",
		"proposed_at", "ratified_at", "activated_at", "supersedes_id",
	},
	"decision": {
		"id", "database_id", "feature", "intent", "target_objects",
		"policy_id", "policy_version", "verdict", "risk_tier", "reason",
		"guardrails", "off_window_ok", "deadline_kind", "deadline_hard_at",
		"evidence", "evidence_id", "action_log_id", "queue_id", "created_at",
		"resolved_at",
	},
	"change_lease": {
		"id", "database_id", "object_key", "decision_id", "holder", "intent",
		"acquired_at", "expires_at", "released_at", "state",
	},
	"schema_baseline": {
		"id", "database_id", "object_identity", "object_type",
		"authorized_definition_hash", "observed_definition_hash",
		"last_authorized_decision_id", "last_authorized_action_id",
		"reconciliation_status", "observed_at", "updated_at",
	},
	"toil_model": {
		"action_type", "model_version", "base_minutes", "notes", "provenance",
		"effective_from", "effective_to",
	},
	"incident_avoided": {
		"id", "database_id", "kind", "severity", "credited_minutes",
		"evidence_id", "model_version", "decision_id", "action_log_id",
		"verification_id", "occurred_at",
	},
	"verification": {
		"id", "database_id", "decision_id", "action_log_id", "criterion",
		"baseline", "minimum_samples", "next_evaluation_at", "hard_deadline_at",
		"verdict", "reason", "created_at", "updated_at", "completed_at",
	},
	"retention_run": {
		"id", "database_id", "schema_name", "table_name", "retention_column",
		"cutoff_at", "candidate_rows", "deleted_rows", "disposition", "created_at",
	},
}

var toilSeedMinutes = map[string]float64{
	"analyze_table":             15,
	"create_index_concurrently": 45,
	"drop_unused_index":         20,
	"reindex_concurrently":      30,
	"vacuum_table":              20,
	"freeze_table":              20,
	"set_table_autovacuum":      30,
	"alter_system_guc":          25,
	"create_statistics":         30,
	"apply_query_hint":          25,
	"online_migration":          120,
	"fk_supporting_index":       30,
	"retention_policy_setup":    60,
	"slot_bound":                30,
	"slot_drop":                 30,
}

func TestAgentNativeNumberedMigrationRegistry(t *testing.T) {
	migrations := agentNativeMigrations()
	if len(migrations) < 4 {
		t.Fatalf("agent-native migrations = %d, want policy/ledger/value/verify", len(migrations))
	}
	seenVersions := make(map[int]bool, len(migrations))
	seenNames := make(map[string]bool, len(migrations))
	previous := 0
	for _, migration := range migrations {
		if migration.Version <= previous || seenVersions[migration.Version] {
			t.Fatalf("migration version %d is not unique and increasing", migration.Version)
		}
		if strings.TrimSpace(migration.Name) == "" || seenNames[migration.Name] {
			t.Fatalf("migration name %q is empty or duplicated", migration.Name)
		}
		if strings.TrimSpace(migration.SQL) == "" {
			t.Fatalf("migration %d %q has empty SQL", migration.Version, migration.Name)
		}
		seenVersions[migration.Version], seenNames[migration.Name] = true, true
		previous = migration.Version
	}
}

func TestAgentNativeMigrationSQLIsIdempotentAndNonDestructive(t *testing.T) {
	for _, migration := range agentNativeMigrations() {
		upper := strings.ToUpper(migration.SQL)
		for _, forbidden := range []string{"DROP TABLE", "DROP COLUMN", "TRUNCATE"} {
			if strings.Contains(upper, forbidden) {
				t.Errorf("migration %d contains destructive %q", migration.Version, forbidden)
			}
		}
		for _, statement := range strings.Split(migration.SQL, ";") {
			assertIdempotentAgentNativeStatement(t, migration.Version, statement)
		}
	}
}

func assertIdempotentAgentNativeStatement(t *testing.T, version int, sql string) {
	t.Helper()
	upper := strings.ToUpper(strings.TrimSpace(sql))
	if upper == "" || strings.HasPrefix(upper, "COMMENT ON") {
		return
	}
	idempotent := strings.Contains(upper, "IF NOT EXISTS") ||
		strings.Contains(upper, "ON CONFLICT") ||
		strings.HasPrefix(upper, "CREATE OR REPLACE VIEW") ||
		strings.HasPrefix(upper, "ALTER TABLE") && strings.Contains(upper, "IF EXISTS")
	if !idempotent {
		t.Errorf("migration %d statement is not visibly idempotent: %s", version, upper)
	}
}

func TestAgentNativeBootstrapCreatesAllTablesAndValueView(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for table := range agentNativeTableColumns {
		assertRelationKind(t, ctx, pool, table, "r")
	}
	assertRelationKind(t, ctx, pool, "value_rollup", "v")
}

func assertRelationKind(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, wantKind string,
) {
	t.Helper()
	var kind string
	err := pool.QueryRow(ctx, `
		SELECT c.relkind::text
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'sage' AND c.relname = $1`, name).Scan(&kind)
	if err != nil {
		t.Fatalf("sage.%s missing: %v", name, err)
	}
	if kind != wantKind {
		t.Fatalf("sage.%s relkind = %q, want %q", name, kind, wantKind)
	}
}

func TestAgentNativeTablesExposeRequiredColumns(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for table, required := range agentNativeTableColumns {
		columns := relationColumns(t, ctx, pool, table)
		for _, column := range required {
			if !columns[column] {
				t.Errorf("sage.%s missing column %q", table, column)
			}
		}
	}
}

func relationColumns(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string,
) map[string]bool {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = $1`, table)
	if err != nil {
		t.Fatalf("read sage.%s columns: %v", table, err)
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan sage.%s column: %v", table, err)
		}
		columns[column] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sage.%s columns: %v", table, err)
	}
	return columns
}

func TestAgentNativeActionLogAddsNullableValueLinks(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	required := []string{
		"decision_id", "verification_id", "toil_minutes_saved", "toil_model_version",
	}
	columns := relationColumns(t, ctx, pool, "action_log")
	for _, column := range required {
		if !columns[column] {
			t.Errorf("sage.action_log missing column %q", column)
		}
	}
	assertActionLogValueNullAndZeroSemantics(t, ctx, pool)
}

func assertActionLogValueNullAndZeroSemantics(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO sage.action_log (action_type, sql_executed, outcome)
		VALUES ('agent_native_test', 'SELECT 1', 'pending') RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("insert legacy-shaped action: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM sage.action_log WHERE id=$1", id) })
	var saved *float64
	if err := pool.QueryRow(ctx, `SELECT toil_minutes_saved FROM sage.action_log WHERE id=$1`,
		id).Scan(&saved); err != nil || saved != nil {
		t.Fatalf("new action saved minutes = %v, err=%v; want NULL", saved, err)
	}
	_, err = pool.Exec(ctx, `UPDATE sage.action_log SET toil_minutes_saved=0 WHERE id=$1`, id)
	if err != nil {
		t.Fatalf("stamp verified zero: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT toil_minutes_saved FROM sage.action_log WHERE id=$1`,
		id).Scan(&saved); err != nil || saved == nil || *saved != 0 {
		t.Fatalf("verified zero = %v, err=%v; want numeric zero", saved, err)
	}
}

func TestAgentNativeForeignKeysLinkEvidenceHistory(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	want := map[string][]string{
		"policy":           {"supersedes_id"},
		"decision":         {"policy_id", "action_log_id", "queue_id"},
		"change_lease":     {"decision_id"},
		"schema_baseline":  {"last_authorized_decision_id", "last_authorized_action_id"},
		"incident_avoided": {"decision_id", "action_log_id", "verification_id"},
		"verification":     {"decision_id", "action_log_id"},
	}
	for table, columns := range want {
		for _, column := range columns {
			if !hasForeignKey(t, ctx, pool, table, column) {
				t.Errorf("sage.%s.%s lacks foreign key", table, column)
			}
		}
	}
}

func hasForeignKey(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, column string,
) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_constraint con
			JOIN pg_class rel ON rel.oid = con.conrelid
			JOIN pg_namespace n ON n.oid = rel.relnamespace
			JOIN unnest(con.conkey) key(attnum) ON true
			JOIN pg_attribute a ON a.attrelid = rel.oid AND a.attnum = key.attnum
			WHERE n.nspname='sage' AND rel.relname=$1
			  AND con.contype='f' AND a.attname=$2)`, table, column).Scan(&exists)
	if err != nil {
		t.Fatalf("inspect foreign key sage.%s.%s: %v", table, column, err)
	}
	return exists
}

func TestAgentNativeIndexesEnforceActiveUniqueness(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	wants := map[string][]string{
		"policy":       {"database_id", "status", "unique"},
		"decision":     {"evidence_id", "unique"},
		"change_lease": {"database_id", "object_key", "state", "unique"},
		"toil_model":   {"action_type", "model_version", "unique"},
	}
	for table, terms := range wants {
		definition := tableIndexDefinitions(t, ctx, pool, table)
		for _, term := range terms {
			if !strings.Contains(strings.ToLower(definition), term) {
				t.Errorf("sage.%s indexes missing %q: %s", table, term, definition)
			}
		}
	}
}

func tableIndexDefinitions(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string,
) string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname='sage' AND tablename=$1 ORDER BY indexname`, table)
	if err != nil {
		t.Fatalf("read sage.%s indexes: %v", table, err)
	}
	defer rows.Close()
	var definitions []string
	for rows.Next() {
		var definition string
		if err := rows.Scan(&definition); err != nil {
			t.Fatalf("scan sage.%s index: %v", table, err)
		}
		definitions = append(definitions, definition)
	}
	return strings.Join(definitions, "\n")
}

func TestAgentNativeToilModelSeedsAreExact(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for actionType, want := range toilSeedMinutes {
		var got float64
		err := pool.QueryRow(ctx, `
			SELECT base_minutes FROM sage.toil_model
			WHERE action_type=$1 AND model_version=1 AND effective_to IS NULL`,
			actionType).Scan(&got)
		if err != nil {
			t.Errorf("seed %s missing: %v", actionType, err)
		} else if got != want {
			t.Errorf("seed %s = %.2f minutes, want %.2f", actionType, got, want)
		}
	}
}

func TestAgentNativeValueRollupSeparatesHonestOutcomes(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	columns := relationColumns(t, ctx, pool, "value_rollup")
	want := []string{
		"day", "database_id", "action_type", "actions_verified",
		"toil_minutes_saved", "actions_reverted", "potential_minutes_pending",
		"incidents_avoided", "incident_minutes_credited",
	}
	for _, column := range want {
		if !columns[column] {
			t.Errorf("sage.value_rollup missing honest-value column %q", column)
		}
	}
}

func TestAgentNativeBootstrapIsIdempotentAndPreservesSeedEdits(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	_, err := pool.Exec(ctx, `UPDATE sage.toil_model SET base_minutes=99
		WHERE action_type='analyze_table' AND model_version=1`)
	if err != nil {
		t.Fatalf("customize toil seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `UPDATE sage.toil_model SET base_minutes=15
			WHERE action_type='analyze_table' AND model_version=1`)
	})
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	var got float64
	if err := pool.QueryRow(ctx, `SELECT base_minutes FROM sage.toil_model
		WHERE action_type='analyze_table' AND model_version=1`).Scan(&got); err != nil {
		t.Fatalf("read customized toil seed: %v", err)
	}
	if got != 99 {
		t.Fatalf("repeat bootstrap overwrote customized seed: got %.2f", got)
	}
}

func TestAgentNativeBootstrapIsConcurrentSafe(t *testing.T) {
	pool, ctx := requireDB(t)
	serializeAcrossPackages(t, ctx, pool)
	const workers = 4
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Bootstrap(ctx, pool)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Bootstrap: %v", err)
		}
	}
	for table := range agentNativeTableColumns {
		assertRelationKind(t, ctx, pool, table, "r")
	}
}

func TestAgentNativeUpgradePreservesActionAndQueueHistory(t *testing.T) {
	pool, ctx := requireDB(t)
	serializeAcrossPackages(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	actionID, queueID := insertLegacyHistory(t, ctx, pool)
	removeAgentNativeSchema(t, ctx, pool)
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("upgrade Bootstrap: %v", err)
	}
	assertHistoryRow(t, ctx, pool, "action_log", actionID)
	assertHistoryRow(t, ctx, pool, "action_queue", queueID)
	assertLegacyActionLinksNull(t, ctx, pool, actionID)
	bootstrapWithRetry(t, ctx, pool)
}

func insertLegacyHistory(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) (int64, int64) {
	t.Helper()
	var actionID, queueID int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome) VALUES
		('legacy_agent_native_test', 'SELECT 1', 'success') RETURNING id`).Scan(&actionID)
	if err != nil {
		t.Fatalf("insert legacy action: %v", err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO sage.action_queue
		(proposed_sql, action_risk, status, action_log_id) VALUES
		('SELECT 1', 'safe', 'approved', $1) RETURNING id`, actionID).Scan(&queueID)
	if err != nil {
		t.Fatalf("insert legacy queue row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.action_queue WHERE id=$1", queueID)
		_, _ = pool.Exec(ctx, "DELETE FROM sage.action_log WHERE id=$1", actionID)
	})
	return actionID, queueID
}

func removeAgentNativeSchema(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) {
	t.Helper()
	statements := []string{
		"DROP VIEW IF EXISTS sage.value_rollup",
		"DROP TABLE IF EXISTS sage.incident_avoided CASCADE",
		"DROP TABLE IF EXISTS sage.verification CASCADE",
		"DROP TABLE IF EXISTS sage.change_lease CASCADE",
		"DROP TABLE IF EXISTS sage.schema_baseline CASCADE",
		"DROP TABLE IF EXISTS sage.decision CASCADE",
		"DROP TABLE IF EXISTS sage.policy CASCADE",
		"DROP TABLE IF EXISTS sage.toil_model CASCADE",
		"DROP TABLE IF EXISTS sage.schema_migrations CASCADE",
		`ALTER TABLE sage.action_log DROP COLUMN IF EXISTS decision_id,
			DROP COLUMN IF EXISTS verification_id,
			DROP COLUMN IF EXISTS toil_minutes_saved,
			DROP COLUMN IF EXISTS toil_model_version`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("prepare previous-schema fixture with %q: %v", statement, err)
		}
	}
}

func assertHistoryRow(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, id int64,
) {
	t.Helper()
	var count int
	query := fmt.Sprintf("SELECT count(*) FROM sage.%s WHERE id=$1", table)
	if err := pool.QueryRow(ctx, query, id).Scan(&count); err != nil {
		t.Fatalf("read sage.%s history: %v", table, err)
	}
	if count != 1 {
		t.Fatalf("sage.%s history rows = %d, want 1", table, count)
	}
}

func assertLegacyActionLinksNull(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, actionID int64,
) {
	t.Helper()
	var decisionID, verificationID *int64
	var minutes *float64
	var modelVersion *int
	err := pool.QueryRow(ctx, `SELECT decision_id, verification_id,
		toil_minutes_saved, toil_model_version FROM sage.action_log WHERE id=$1`,
		actionID).Scan(&decisionID, &verificationID, &minutes, &modelVersion)
	if err != nil {
		t.Fatalf("read migrated legacy action: %v", err)
	}
	if decisionID != nil || verificationID != nil || minutes != nil || modelVersion != nil {
		t.Fatalf("legacy action received invented evidence/value: %v %v %v %v",
			decisionID, verificationID, minutes, modelVersion)
	}
}
