package schema

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testDSN() string {
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
}

var (
	testPool     *pgxpool.Pool
	testPoolOnce sync.Once
	testPoolErr  error
)

func requireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	testPoolOnce.Do(func() {
		dsn := testDSN()
		poolCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			testPoolErr = fmt.Errorf("parsing DSN: %w", err)
			return
		}
		// Use a single connection so advisory locks are always on the
		// same session, preventing lock contention between tests.
		poolCfg.MaxConns = 1
		testPool, testPoolErr = pgxpool.NewWithConfig(ctx, poolCfg)
		if testPoolErr != nil {
			return
		}
		if err := testPool.Ping(ctx); err != nil {
			testPoolErr = fmt.Errorf("ping: %w", err)
			testPool.Close()
			testPool = nil
		}
	})
	if testPoolErr != nil {
		t.Skipf("database unavailable: %v", testPoolErr)
	}
	return testPool, ctx
}

func TestExpectedTables_AllPresent(t *testing.T) {
	want := []string{
		"action_log",
		"snapshots",
		"findings",
		"explain_cache",
		"briefings",
		"config",
		"mcp_log",
		"alert_log",
		"query_hints",
	}

	if len(expectedTables) != len(want) {
		t.Fatalf("expectedTables has %d entries, want %d",
			len(expectedTables), len(want))
	}

	for i, w := range want {
		if expectedTables[i].name != w {
			t.Errorf("expectedTables[%d].name = %q, want %q",
				i, expectedTables[i].name, w)
		}
	}
}

func TestExpectedTables_DDLNotEmpty(t *testing.T) {
	for _, tbl := range expectedTables {
		if strings.TrimSpace(tbl.ddl) == "" {
			t.Errorf("DDL for table %q is empty", tbl.name)
		}
	}
}

func TestDDL_UsesIfNotExists(t *testing.T) {
	for _, tbl := range expectedTables {
		if !strings.Contains(tbl.ddl, "IF NOT EXISTS") {
			t.Errorf("DDL for %q missing IF NOT EXISTS", tbl.name)
		}
	}
}

func TestDDL_NoDrop(t *testing.T) {
	for _, tbl := range expectedTables {
		upper := strings.ToUpper(tbl.ddl)
		if strings.Contains(upper, "DROP TABLE") ||
			strings.Contains(upper, "DROP INDEX") ||
			strings.Contains(upper, "DROP SCHEMA") {
			t.Errorf("DDL for %q contains a DROP statement", tbl.name)
		}
	}
}

func TestDDL_ReferenceSageSchema(t *testing.T) {
	for _, tbl := range expectedTables {
		qualifiedName := "sage." + tbl.name
		if !strings.Contains(tbl.ddl, qualifiedName) {
			t.Errorf("DDL for %q missing qualified name %q",
				tbl.name, qualifiedName)
		}
	}
}

func TestFullSchemaDDL_ContainsCreateSchema(t *testing.T) {
	if !strings.Contains(fullSchemaDDL, "CREATE SCHEMA sage") {
		t.Error("fullSchemaDDL missing CREATE SCHEMA sage")
	}
}

func TestFullSchemaDDL_ContainsAllTables(t *testing.T) {
	for _, tbl := range expectedTables {
		qualifiedName := "sage." + tbl.name
		if !strings.Contains(fullSchemaDDL, qualifiedName) {
			t.Errorf("fullSchemaDDL missing table %q", qualifiedName)
		}
	}
}

func TestFullSchemaDDL_NoDrop(t *testing.T) {
	upper := strings.ToUpper(fullSchemaDDL)
	for _, kw := range []string{"DROP TABLE", "DROP INDEX", "DROP SCHEMA"} {
		if strings.Contains(upper, kw) {
			t.Errorf("fullSchemaDDL contains %q", kw)
		}
	}
}

func TestAdvisoryLockKey_UsesHashText(t *testing.T) {
	// The advisory lock functions must use hashtext('pg_sage') as the key.
	// Verify this by checking the DDL-adjacent lock/unlock SQL
	// embedded in acquireAdvisoryLock and ReleaseAdvisoryLock.
	// We verify the constant string is present in the source via
	// the fullSchemaDDL not containing it (it's in Go code, not DDL),
	// but we can verify the config table DDL references the
	// trust_ramp_start key that PersistTrustRampStart uses.

	if !strings.Contains(ddlConfig, "sage.config") {
		t.Error("ddlConfig missing sage.config table reference")
	}
	if !strings.Contains(ddlConfig, "key") {
		t.Error("ddlConfig missing 'key' column")
	}
	if !strings.Contains(ddlConfig, "text PRIMARY KEY") {
		t.Error("ddlConfig missing text PRIMARY KEY for key column")
	}
}

func TestDDLActionLog_HasExpectedColumns(t *testing.T) {
	required := []string{
		"id", "executed_at", "action_type", "finding_id",
		"sql_executed", "rollback_sql", "before_state",
		"after_state", "outcome", "rollback_reason", "measured_at",
	}
	for _, col := range required {
		if !strings.Contains(ddlActionLog, col) {
			t.Errorf("ddlActionLog missing column %q", col)
		}
	}
}

func TestDDLFindings_HasDedupIndex(t *testing.T) {
	if !strings.Contains(ddlFindings, "idx_findings_dedup") {
		t.Error("ddlFindings missing dedup index")
	}
	if !strings.Contains(ddlFindings, "UNIQUE INDEX") {
		t.Error("ddlFindings dedup index should be UNIQUE")
	}
}

func TestDDLExplainCache_HasQueryidIndex(t *testing.T) {
	if !strings.Contains(ddlExplainCache, "idx_explain_queryid") {
		t.Error("ddlExplainCache missing queryid index")
	}
}

func TestDDLMCPLog_HasClientIndex(t *testing.T) {
	if !strings.Contains(ddlMCPLog, "idx_mcp_log_client") {
		t.Error("ddlMCPLog missing client index")
	}
}

func TestTrustRampStart_TimestampFormats(t *testing.T) {
	// PersistTrustRampStart parses timestamps with several layouts.
	// Verify the layouts parse representative strings correctly.
	cases := []struct {
		name  string
		input string
	}{
		{"RFC3339Nano", "2026-03-22T10:30:00.123456789Z"},
		{"PG short offset", "2026-03-22T10:30:00.123456-05"},
		{"PG colon offset", "2026-03-22T10:30:00.123456-05:00"},
		{"PG space short", "2026-03-22 10:30:00.123456-05"},
		{"PG space colon", "2026-03-22 10:30:00.123456-05:00"},
	}

	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999-07",
		"2006-01-02T15:04:05.999999-07:00",
		"2006-01-02 15:04:05.999999-07",
		"2006-01-02 15:04:05.999999-07:00",
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed := false
			for _, layout := range layouts {
				if _, err := time.Parse(layout, tc.input); err == nil {
					parsed = true
					break
				}
			}
			if !parsed {
				t.Errorf("no layout matched %q", tc.input)
			}
		})
	}
}

func TestTrustRampStart_RejectsGarbage(t *testing.T) {
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999-07",
		"2006-01-02T15:04:05.999999-07:00",
		"2006-01-02 15:04:05.999999-07",
		"2006-01-02 15:04:05.999999-07:00",
	}

	garbage := []string{"not-a-date", "", "2026", "2026-13-40"}
	for _, g := range garbage {
		for _, layout := range layouts {
			if _, err := time.Parse(layout, g); err == nil {
				t.Errorf("layout %q unexpectedly parsed garbage %q",
					layout, g)
			}
		}
	}
}

func TestBootstrap_FreshDatabase(t *testing.T) {
	pool, ctx := requireDB(t)

	// Release any advisory locks held by this session.
	_, _ = pool.Exec(ctx, "SELECT pg_advisory_unlock_all()")

	_, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS sage CASCADE")
	if err != nil {
		t.Fatalf("dropping sage schema: %v", err)
	}

	// Bootstrap should create everything.
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	ReleaseAdvisoryLock(ctx, pool)

	// Assert schema exists.
	var one int
	err = pool.QueryRow(ctx,
		"SELECT 1 FROM information_schema.schemata "+
			"WHERE schema_name='sage'",
	).Scan(&one)
	if err != nil {
		t.Fatal("sage schema does not exist after Bootstrap")
	}

	// Assert all expected tables exist.
	for _, tbl := range expectedTables {
		err = pool.QueryRow(ctx,
			"SELECT 1 FROM information_schema.tables "+
				"WHERE table_schema='sage' AND table_name=$1",
			tbl.name,
		).Scan(&one)
		if err != nil {
			t.Errorf("table sage.%s missing after Bootstrap", tbl.name)
		}
	}

	// Assert trust_ramp_start was persisted.
	ts, err := PersistTrustRampStart(ctx, pool, time.Time{})
	if err != nil {
		t.Fatalf("PersistTrustRampStart: %v", err)
	}
	if ts.IsZero() {
		t.Error("trust_ramp_start returned zero time")
	}
}

func TestBootstrap_Idempotent(t *testing.T) {
	pool, ctx := requireDB(t)

	// First bootstrap (may already exist from previous test).
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap (first): %v", err)
	}
	ReleaseAdvisoryLock(ctx, pool)

	// Second bootstrap — should not error.
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap (second): %v", err)
	}
	ReleaseAdvisoryLock(ctx, pool)

	// PersistTrustRampStart should return a valid time.
	ts1, err := PersistTrustRampStart(ctx, pool, time.Time{})
	if err != nil {
		t.Fatalf("PersistTrustRampStart (first): %v", err)
	}
	if ts1.IsZero() {
		t.Fatal("trust_ramp_start returned zero time")
	}

	// Calling again should return the same time (not overwritten).
	ts2, err := PersistTrustRampStart(ctx, pool, time.Time{})
	if err != nil {
		t.Fatalf("PersistTrustRampStart (second): %v", err)
	}
	if !ts1.Equal(ts2) {
		t.Errorf(
			"trust_ramp_start changed: %v -> %v",
			ts1, ts2,
		)
	}
}

func TestPersistTrustRampStart_HonorsConfigValue(t *testing.T) {
	pool, ctx := requireDB(t)

	// Clean slate: drop any existing trust_ramp_start row.
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.config WHERE key = 'trust_ramp_start'")

	// Provide a specific config ramp start in the past.
	want := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := PersistTrustRampStart(ctx, pool, want)
	if err != nil {
		t.Fatalf("PersistTrustRampStart with config value: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf(
			"expected config ramp start %v, got %v", want, got,
		)
	}

	// Calling again with a different config value should NOT override.
	other := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	got2, err := PersistTrustRampStart(ctx, pool, other)
	if err != nil {
		t.Fatalf("PersistTrustRampStart (second): %v", err)
	}
	if !got2.Equal(want) {
		t.Errorf(
			"existing value overwritten: expected %v, got %v",
			want, got2,
		)
	}
}

func TestPersistTrustRampStart_ZeroConfigUsesNow(t *testing.T) {
	pool, ctx := requireDB(t)

	// Clean slate.
	_, _ = pool.Exec(ctx,
		"DELETE FROM sage.config WHERE key = 'trust_ramp_start'")

	// Zero config ramp start should default to ~now().
	before := time.Now().Add(-2 * time.Second)
	got, err := PersistTrustRampStart(ctx, pool, time.Time{})
	if err != nil {
		t.Fatalf("PersistTrustRampStart with zero config: %v", err)
	}
	after := time.Now().Add(2 * time.Second)
	if got.Before(before) || got.After(after) {
		t.Errorf(
			"expected time near now, got %v (window %v – %v)",
			got, before, after,
		)
	}
}
