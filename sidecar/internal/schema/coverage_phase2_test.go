package schema

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// MigrateConfigSchema / addConfigColumns / addConfigCompositeIndex /
// ensureConfigAudit — all 0% coverage
// These require a live Postgres connection. Skip if unavailable.
// ---------------------------------------------------------------------------

func TestPhase2_MigrateConfigSchema_FullRun(t *testing.T) {
	pool, ctx := requireDB(t)
	serializeAcrossPackages(t, ctx, pool)

	// Acquire lock before dropping schema to prevent cross-package races.
	_, _ = pool.Exec(ctx, "SELECT pg_advisory_lock(hashtext('pg_sage'))")
	_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS sage CASCADE")
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	ReleaseAdvisoryLock(ctx, pool)

	// Run MigrateConfigSchema — should succeed on fresh schema.
	if err := MigrateConfigSchema(ctx, pool); err != nil {
		t.Fatalf("MigrateConfigSchema: %v", err)
	}

	// Verify database_id column exists on sage.config.
	var colCount int
	err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM information_schema.columns "+
			"WHERE table_schema='sage' AND table_name='config' "+
			"AND column_name='database_id'",
	).Scan(&colCount)
	if err != nil {
		t.Fatalf("querying database_id column: %v", err)
	}
	if colCount != 1 {
		t.Errorf("expected database_id column to exist, count=%d", colCount)
	}

	// Verify updated_by_user_id column exists on sage.config.
	err = pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM information_schema.columns "+
			"WHERE table_schema='sage' AND table_name='config' "+
			"AND column_name='updated_by_user_id'",
	).Scan(&colCount)
	if err != nil {
		t.Fatalf("querying updated_by_user_id column: %v", err)
	}
	if colCount != 1 {
		t.Errorf("expected updated_by_user_id column, count=%d", colCount)
	}

	// Verify config_audit table exists.
	var one int
	err = pool.QueryRow(ctx,
		"SELECT 1 FROM information_schema.tables "+
			"WHERE table_schema='sage' AND table_name='config_audit'",
	).Scan(&one)
	if err != nil {
		t.Error("sage.config_audit table missing after MigrateConfigSchema")
	}
}

func TestPhase2_MigrateConfigSchema_Idempotent(t *testing.T) {
	pool, ctx := requireDB(t)

	// Ensure schema exists.
	_, _ = pool.Exec(ctx, "SELECT pg_advisory_unlock_all()")
	bootstrapWithRetry(t, ctx, pool)
	ReleaseAdvisoryLock(ctx, pool)

	// Run MigrateConfigSchema twice — second run should not error.
	if err := MigrateConfigSchema(ctx, pool); err != nil {
		t.Fatalf("MigrateConfigSchema (first): %v", err)
	}
	if err := MigrateConfigSchema(ctx, pool); err != nil {
		t.Fatalf("MigrateConfigSchema (second): %v", err)
	}
}

func TestPhase2_MigrateConfigSchema_CompositeIndex(t *testing.T) {
	pool, ctx := requireDB(t)

	// Ensure schema + migration have run.
	_, _ = pool.Exec(ctx, "SELECT pg_advisory_unlock_all()")
	bootstrapWithRetry(t, ctx, pool)
	ReleaseAdvisoryLock(ctx, pool)

	if err := MigrateConfigSchema(ctx, pool); err != nil {
		t.Fatalf("MigrateConfigSchema: %v", err)
	}

	// Verify composite index exists.
	var idxCount int
	err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM pg_indexes "+
			"WHERE schemaname='sage' AND tablename='config' "+
			"AND indexname='idx_config_key_db'",
	).Scan(&idxCount)
	if err != nil {
		t.Fatalf("querying composite index: %v", err)
	}
	if idxCount != 1 {
		t.Errorf("expected idx_config_key_db index, count=%d", idxCount)
	}
}

// ---------------------------------------------------------------------------
// EnsureDatabasesTable (0% coverage)
// ---------------------------------------------------------------------------

func TestPhase2_EnsureDatabasesTable_Creates(t *testing.T) {
	pool, ctx := requireDB(t)

	// Ensure sage schema exists but drop databases table.
	_, _ = pool.Exec(ctx, "SELECT pg_advisory_unlock_all()")
	bootstrapWithRetry(t, ctx, pool)
	ReleaseAdvisoryLock(ctx, pool)
	// Cross-pkg lock acquired AFTER unlock_all so it survives.
	serializeAcrossPackages(t, ctx, pool)

	// Drop the databases table so EnsureDatabasesTable can recreate it.
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS sage.databases CASCADE")

	if err := EnsureDatabasesTable(ctx, pool); err != nil {
		t.Fatalf("EnsureDatabasesTable: %v", err)
	}

	// Verify it exists.
	var one int
	err := pool.QueryRow(ctx,
		"SELECT 1 FROM information_schema.tables "+
			"WHERE table_schema='sage' AND table_name='databases'",
	).Scan(&one)
	if err != nil {
		t.Error("sage.databases missing after EnsureDatabasesTable")
	}
}

func TestPhase2_EnsureDatabasesTable_Idempotent(t *testing.T) {
	pool, ctx := requireDB(t)

	_, _ = pool.Exec(ctx, "SELECT pg_advisory_unlock_all()")
	bootstrapWithRetry(t, ctx, pool)
	ReleaseAdvisoryLock(ctx, pool)

	// Run twice — second call should not error.
	if err := EnsureDatabasesTable(ctx, pool); err != nil {
		t.Fatalf("EnsureDatabasesTable (first): %v", err)
	}
	if err := EnsureDatabasesTable(ctx, pool); err != nil {
		t.Fatalf("EnsureDatabasesTable (second): %v", err)
	}
}

func TestPhase2_EnsureDatabasesTable_ExpectedColumns(t *testing.T) {
	pool, ctx := requireDB(t)

	_, _ = pool.Exec(ctx, "SELECT pg_advisory_unlock_all()")
	bootstrapWithRetry(t, ctx, pool)
	ReleaseAdvisoryLock(ctx, pool)

	if err := EnsureDatabasesTable(ctx, pool); err != nil {
		t.Fatalf("EnsureDatabasesTable: %v", err)
	}

	// Verify key columns exist.
	wantCols := []string{
		"id", "name", "host", "port", "database_name",
		"username", "password_enc", "sslmode",
		"max_connections", "enabled", "tags",
		"trust_level", "execution_mode",
		"created_at", "created_by", "updated_at",
	}
	for _, col := range wantCols {
		var cnt int
		err := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM information_schema.columns "+
				"WHERE table_schema='sage' AND table_name='databases' "+
				"AND column_name=$1", col,
		).Scan(&cnt)
		if err != nil {
			t.Errorf("querying column %s: %v", col, err)
		} else if cnt != 1 {
			t.Errorf("column %s missing from sage.databases", col)
		}
	}
}

// ---------------------------------------------------------------------------
// ensureTablesExist (66.7% coverage — need to exercise missing-table path)
// ---------------------------------------------------------------------------

func TestPhase2_EnsureTablesExist_RecreatesMissing(t *testing.T) {
	pool, ctx := requireDB(t)
	serializeAcrossPackages(t, ctx, pool)

	// Start with a full bootstrap under advisory lock.
	_, _ = pool.Exec(ctx, "SELECT pg_advisory_lock(hashtext('pg_sage'))")
	_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS sage CASCADE")
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	ReleaseAdvisoryLock(ctx, pool)

	// Drop one table to simulate a partially-present schema.
	_, err := pool.Exec(ctx, "DROP TABLE IF EXISTS sage.briefings CASCADE")
	if err != nil {
		t.Fatalf("dropping briefings: %v", err)
	}

	// ensureTablesExist should recreate it.
	if err := ensureTablesExist(ctx, pool); err != nil {
		t.Fatalf("ensureTablesExist: %v", err)
	}

	// Verify briefings table exists again.
	var one int
	err = pool.QueryRow(ctx,
		"SELECT 1 FROM information_schema.tables "+
			"WHERE table_schema='sage' AND table_name='briefings'",
	).Scan(&one)
	if err != nil {
		t.Error("sage.briefings not recreated by ensureTablesExist")
	}
}

func TestPhase2_EnsureTablesExist_AllPresent(t *testing.T) {
	pool, ctx := requireDB(t)

	_, _ = pool.Exec(ctx, "SELECT pg_advisory_unlock_all()")
	bootstrapWithRetry(t, ctx, pool)
	ReleaseAdvisoryLock(ctx, pool)

	// All tables exist — should succeed without error.
	if err := ensureTablesExist(ctx, pool); err != nil {
		t.Fatalf("ensureTablesExist with all tables present: %v", err)
	}
}

func TestPhase2_EnsureTablesExist_RecreatesMultipleMissing(t *testing.T) {
	pool, ctx := requireDB(t)
	serializeAcrossPackages(t, ctx, pool)

	// Acquire lock before dropping schema to prevent cross-package races.
	_, _ = pool.Exec(ctx, "SELECT pg_advisory_lock(hashtext('pg_sage'))")
	_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS sage CASCADE")
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	ReleaseAdvisoryLock(ctx, pool)

	// Drop multiple tables.
	for _, tbl := range []string{"query_hints", "explain_cache"} {
		_, err := pool.Exec(ctx,
			"DROP TABLE IF EXISTS sage."+tbl+" CASCADE")
		if err != nil {
			t.Fatalf("dropping %s: %v", tbl, err)
		}
	}

	if err := ensureTablesExist(ctx, pool); err != nil {
		t.Fatalf("ensureTablesExist: %v", err)
	}

	// Verify both were recreated.
	for _, tbl := range []string{"query_hints", "explain_cache"} {
		var one int
		err := pool.QueryRow(ctx,
			"SELECT 1 FROM information_schema.tables "+
				"WHERE table_schema='sage' AND table_name=$1", tbl,
		).Scan(&one)
		if err != nil {
			t.Errorf("sage.%s not recreated", tbl)
		}
	}
}

// ---------------------------------------------------------------------------
// DDL constant coverage for config_migration.go
// ---------------------------------------------------------------------------

func TestPhase2_DDLConfigAudit_HasExpectedElements(t *testing.T) {
	if !strings.Contains(ddlConfigAudit, "sage.config_audit") {
		t.Error("ddlConfigAudit missing sage.config_audit")
	}
	if !strings.Contains(ddlConfigAudit, "IF NOT EXISTS") {
		t.Error("ddlConfigAudit missing IF NOT EXISTS")
	}
	cols := []string{
		"id", "key", "old_value", "new_value",
		"database_id", "changed_by", "changed_at",
	}
	for _, col := range cols {
		if !strings.Contains(ddlConfigAudit, col) {
			t.Errorf("ddlConfigAudit missing column %q", col)
		}
	}
}

func TestPhase2_DDLConfigAudit_HasIndexes(t *testing.T) {
	if !strings.Contains(ddlConfigAudit, "idx_config_audit_time") {
		t.Error("ddlConfigAudit missing time index")
	}
	if !strings.Contains(ddlConfigAudit, "idx_config_audit_db") {
		t.Error("ddlConfigAudit missing db index")
	}
}

func TestPhase2_DDLDatabases_HasExpectedElements(t *testing.T) {
	if !strings.Contains(ddlDatabases, "sage.databases") {
		t.Error("ddlDatabases missing sage.databases")
	}
	if !strings.Contains(ddlDatabases, "IF NOT EXISTS") {
		t.Error("ddlDatabases missing IF NOT EXISTS")
	}
	cols := []string{
		"id", "name", "host", "port", "database_name",
		"username", "password_enc", "sslmode",
		"max_connections", "enabled", "tags",
		"trust_level", "execution_mode",
	}
	for _, col := range cols {
		if !strings.Contains(ddlDatabases, col) {
			t.Errorf("ddlDatabases missing column %q", col)
		}
	}
}

func TestPhase2_DDLDatabases_NoDrop(t *testing.T) {
	upper := strings.ToUpper(ddlDatabases)
	if strings.Contains(upper, "DROP TABLE") {
		t.Error("ddlDatabases contains DROP TABLE")
	}
}

// ---------------------------------------------------------------------------
// Notification DDL constants
// ---------------------------------------------------------------------------

func TestPhase2_DDLNotificationChannels_HasExpectedElements(t *testing.T) {
	if !strings.Contains(ddlNotificationChannels, "sage.notification_channels") {
		t.Error("ddlNotificationChannels missing table reference")
	}
	cols := []string{"id", "name", "type", "config", "enabled"}
	for _, col := range cols {
		if !strings.Contains(ddlNotificationChannels, col) {
			t.Errorf("ddlNotificationChannels missing column %q", col)
		}
	}
}

func TestPhase2_DDLNotificationRules_HasExpectedElements(t *testing.T) {
	if !strings.Contains(ddlNotificationRules, "sage.notification_rules") {
		t.Error("ddlNotificationRules missing table reference")
	}
	if !strings.Contains(ddlNotificationRules, "channel_id") {
		t.Error("ddlNotificationRules missing channel_id FK")
	}
}

func TestPhase2_DDLNotificationLog_HasExpectedElements(t *testing.T) {
	if !strings.Contains(ddlNotificationLog, "sage.notification_log") {
		t.Error("ddlNotificationLog missing table reference")
	}
	if !strings.Contains(ddlNotificationLog, "channel_id") {
		t.Error("ddlNotificationLog missing channel_id")
	}
	if !strings.Contains(ddlNotificationLog, "status") {
		t.Error("ddlNotificationLog missing status column")
	}
}

// ---------------------------------------------------------------------------
// Action queue DDL
// ---------------------------------------------------------------------------

func TestPhase2_DDLActionQueue_HasExpectedElements(t *testing.T) {
	if !strings.Contains(ddlActionQueue, "sage.action_queue") {
		t.Error("ddlActionQueue missing table reference")
	}
	cols := []string{
		"id", "database_id", "finding_id", "proposed_sql",
		"rollback_sql", "action_risk", "status",
	}
	for _, col := range cols {
		if !strings.Contains(ddlActionQueue, col) {
			t.Errorf("ddlActionQueue missing column %q", col)
		}
	}
}

// ---------------------------------------------------------------------------
// Migration DDL constants
// ---------------------------------------------------------------------------

func TestPhase2_DDLActionLogApprovalCols_Idempotent(t *testing.T) {
	if !strings.Contains(ddlActionLogApprovalCols, "IF NOT EXISTS") {
		// ALTER TABLE ADD COLUMN IF NOT EXISTS
		if !strings.Contains(ddlActionLogApprovalCols, "ADD COLUMN IF NOT EXISTS") {
			t.Error("ddlActionLogApprovalCols is not idempotent")
		}
	}
	if !strings.Contains(ddlActionLogApprovalCols, "approved_by") {
		t.Error("ddlActionLogApprovalCols missing approved_by")
	}
	if !strings.Contains(ddlActionLogApprovalCols, "approved_at") {
		t.Error("ddlActionLogApprovalCols missing approved_at")
	}
}

func TestPhase2_DDLUsersOAuth_HasExpectedElements(t *testing.T) {
	if !strings.Contains(ddlUsersOAuth, "oauth_provider") {
		t.Error("ddlUsersOAuth missing oauth_provider column")
	}
	if !strings.Contains(ddlUsersOAuth, "DROP NOT NULL") {
		t.Error("ddlUsersOAuth missing DROP NOT NULL for password")
	}
}

// ---------------------------------------------------------------------------
// Bootstrap + runMigrations integration (improves ensureTablesExist path)
// ---------------------------------------------------------------------------

func TestPhase2_Bootstrap_RunsMigrations(t *testing.T) {
	pool, ctx := requireDB(t)
	serializeAcrossPackages(t, ctx, pool)

	// Acquire lock before dropping schema to prevent cross-package races.
	_, _ = pool.Exec(ctx, "SELECT pg_advisory_lock(hashtext('pg_sage'))")
	_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS sage CASCADE")

	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	ReleaseAdvisoryLock(ctx, pool)

	// Verify migration columns exist (approved_by on action_log).
	var cnt int
	err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM information_schema.columns "+
			"WHERE table_schema='sage' AND table_name='action_log' "+
			"AND column_name='approved_by'",
	).Scan(&cnt)
	if err != nil {
		t.Fatalf("querying approved_by: %v", err)
	}
	if cnt != 1 {
		t.Error("approved_by column missing after Bootstrap")
	}

	// Verify oauth_provider on users.
	err = pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM information_schema.columns "+
			"WHERE table_schema='sage' AND table_name='users' "+
			"AND column_name='oauth_provider'",
	).Scan(&cnt)
	if err != nil {
		t.Fatalf("querying oauth_provider: %v", err)
	}
	if cnt != 1 {
		t.Error("oauth_provider column missing after Bootstrap")
	}
}
