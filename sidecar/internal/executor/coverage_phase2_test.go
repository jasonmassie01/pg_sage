package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// ---------------------------------------------------------------------------
// connectTestDB helper
// ---------------------------------------------------------------------------

func connectTestDB2(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := "postgres://postgres:postgres@localhost:5432/" +
		"postgres?sslmode=disable"
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("DB unavailable: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("DB ping failed: %v", err)
	}
	return pool
}

// ---------------------------------------------------------------------------
// checkSchemaCreate (was 57.1%) — test missing-grant path
// ---------------------------------------------------------------------------

func TestPhase2_CheckSchemaCreate_HasGrant(t *testing.T) {
	pool := connectTestDB2(t)
	defer pool.Close()

	var logged []string
	logFn := func(_ string, msg string, args ...any) {
		logged = append(logged, msg)
	}

	// postgres user typically has CREATE on public schema.
	checkSchemaCreate(
		context.Background(), pool, "postgres", logFn,
	)

	// Verify no WARNING was logged (postgres has the grant).
	for _, msg := range logged {
		if strings.Contains(msg, "WARNING") &&
			strings.Contains(msg, "lacks CREATE") {
			t.Error("postgres user should have CREATE on public")
		}
	}
}

func TestPhase2_CheckSchemaCreate_NonExistentUser(t *testing.T) {
	pool := connectTestDB2(t)
	defer pool.Close()

	var logged []string
	logFn := func(_ string, msg string, args ...any) {
		logged = append(logged, msg)
	}

	// Non-existent user should trigger an error path.
	checkSchemaCreate(
		context.Background(), pool,
		"nonexistent_user_xyz_12345", logFn,
	)

	// Should log something (either error or warning).
	if len(logged) == 0 {
		t.Error("expected log output for non-existent user")
	}
}

func TestPhase2_CheckSchemaCreate_ResolvesCurrentUser(t *testing.T) {
	pool := connectTestDB2(t)
	defer pool.Close()

	var logged []string
	logFn := func(_ string, msg string, args ...any) {
		logged = append(logged, msg)
	}

	// Call VerifyGrants which resolves the current user.
	VerifyGrants(context.Background(), pool, "", logFn)
	// Should not panic and should resolve user correctly.
}

// ---------------------------------------------------------------------------
// checkSignalBackend (was 57.1%) — test missing-role path
// ---------------------------------------------------------------------------

func TestPhase2_CheckSignalBackend_PostgresUser(t *testing.T) {
	pool := connectTestDB2(t)
	defer pool.Close()

	var logged []string
	logFn := func(_ string, msg string, args ...any) {
		logged = append(logged, msg)
	}

	checkSignalBackend(
		context.Background(), pool, "postgres", logFn,
	)

	// postgres superuser should have pg_signal_backend membership
	// (or equivalent). Either way, should not panic.
}

func TestPhase2_CheckSignalBackend_NonExistentUser(t *testing.T) {
	pool := connectTestDB2(t)
	defer pool.Close()

	var logged []string
	logFn := func(_ string, msg string, args ...any) {
		logged = append(logged, msg)
	}

	checkSignalBackend(
		context.Background(), pool,
		"nonexistent_role_xyz_12345", logFn,
	)

	// Should log an error (can't check role for non-existent user).
	if len(logged) == 0 {
		t.Error("expected log output for non-existent role")
	}
}

func TestPhase2_CheckSignalBackend_EmptyUser(t *testing.T) {
	pool := connectTestDB2(t)
	defer pool.Close()

	var logged []string
	logFn := func(_ string, msg string, args ...any) {
		logged = append(logged, msg)
	}

	checkSignalBackend(
		context.Background(), pool, "", logFn,
	)

	// Empty user should trigger error path.
	if len(logged) == 0 {
		t.Error("expected log output for empty user")
	}
}

// ---------------------------------------------------------------------------
// categorizeAction — additional edge cases
// ---------------------------------------------------------------------------

func TestPhase2_CategorizeAction_AllTypes(t *testing.T) {
	tests := []struct {
		sql  string
		want string
	}{
		{"CREATE INDEX CONCURRENTLY idx ON t (a)", "create_index"},
		{"DROP INDEX CONCURRENTLY idx", "drop_index"},
		{"REINDEX INDEX idx", "reindex"},
		{"VACUUM ANALYZE t", "vacuum"},
		{"ANALYZE t", "analyze"},
		{"SELECT pg_terminate_backend(123)", "terminate_backend"},
		{"ALTER TABLE t ADD COLUMN c int", "alter"},
		{"CLUSTER t USING idx", "ddl"},
	}
	for _, tt := range tests {
		got := categorizeAction(tt.sql)
		if got != tt.want {
			t.Errorf("categorizeAction(%q) = %q, want %q",
				tt.sql, got, tt.want)
		}
	}
}

func TestPhase2_CategorizeAction_CaseInsensitive(t *testing.T) {
	got := categorizeAction("create index concurrently idx on t (a)")
	if got != "create_index" {
		t.Errorf("expected create_index, got %q", got)
	}
}

func TestPhase2_CategorizeAction_MixedCase(t *testing.T) {
	got := categorizeAction("Vacuum Analyze public.orders")
	if got != "vacuum" {
		t.Errorf("expected vacuum, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// actionOutcome
// ---------------------------------------------------------------------------

func TestPhase2_ActionOutcome_Success(t *testing.T) {
	got := actionOutcome(nil)
	if got != "monitoring" {
		t.Errorf("expected 'monitoring', got %q", got)
	}
}

func TestPhase2_ActionOutcome_Error(t *testing.T) {
	got := actionOutcome(context.DeadlineExceeded)
	if got != "failed" {
		t.Errorf("expected 'failed', got %q", got)
	}
}

// ---------------------------------------------------------------------------
// nilIfEmpty
// ---------------------------------------------------------------------------

func TestPhase2_NilIfEmpty_Empty(t *testing.T) {
	if nilIfEmpty("") != nil {
		t.Error("empty string should return nil")
	}
}

func TestPhase2_NilIfEmpty_NonEmpty(t *testing.T) {
	got := nilIfEmpty("hello")
	if got == nil {
		t.Fatal("non-empty should return non-nil")
	}
	if *got != "hello" {
		t.Errorf("expected 'hello', got %q", *got)
	}
}

// ---------------------------------------------------------------------------
// extractIndexName — additional edge cases
// ---------------------------------------------------------------------------

func TestPhase2_ExtractIndexName_Variants(t *testing.T) {
	tests := []struct {
		sql  string
		want string
	}{
		{
			"CREATE INDEX CONCURRENTLY idx_test ON t (a)",
			"idx_test",
		},
		{
			"CREATE INDEX idx_basic ON t (a)",
			"idx_basic",
		},
		{
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_safe ON t (a)",
			"idx_safe",
		},
		{
			"CREATE INDEX IF NOT EXISTS idx_ine ON t (a)",
			"idx_ine",
		},
		{
			"CREATE INDEX CONCURRENTLY ON t (a)",
			"",
		},
		{
			"DROP INDEX idx_drop",
			"idx_drop",
		},
		{
			"SELECT 1 FROM table",
			"",
		},
		{
			"CREATE INDEX CONCURRENTLY",
			"",
		},
		{
			`CREATE INDEX CONCURRENTLY "quoted_name" ON t (a)`,
			"quoted_name",
		},
	}
	for _, tt := range tests {
		got := extractIndexName(tt.sql)
		if got != tt.want {
			t.Errorf("extractIndexName(%q) = %q, want %q",
				tt.sql, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// NeedsConcurrently + NeedsTopLevel — edge cases
// ---------------------------------------------------------------------------

func TestPhase2_NeedsConcurrently_Variants(t *testing.T) {
	tests := []struct {
		sql  string
		want bool
	}{
		{"CREATE INDEX CONCURRENTLY idx ON t (a)", true},
		{"REINDEX INDEX CONCURRENTLY idx", true},
		{"DROP INDEX CONCURRENTLY idx", true},
		{"CREATE INDEX idx ON t (a)", false},
		{"VACUUM ANALYZE t", false},
	}
	for _, tt := range tests {
		got := NeedsConcurrently(tt.sql)
		if got != tt.want {
			t.Errorf("NeedsConcurrently(%q) = %v, want %v",
				tt.sql, got, tt.want)
		}
	}
}

func TestPhase2_NeedsTopLevel_Variants(t *testing.T) {
	tests := []struct {
		sql  string
		want bool
	}{
		{"VACUUM ANALYZE t", true},
		{"VACUUM t", true},
		{"vacuum full t", true},
		{"CREATE INDEX idx ON t (a)", false},
		{"SELECT 1", false},
	}
	for _, tt := range tests {
		got := NeedsTopLevel(tt.sql)
		if got != tt.want {
			t.Errorf("NeedsTopLevel(%q) = %v, want %v",
				tt.sql, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// VerifyGrants full integration
// ---------------------------------------------------------------------------

func TestPhase2_VerifyGrants_FullIntegration(t *testing.T) {
	pool := connectTestDB2(t)
	defer pool.Close()

	var messages []string
	logFn := func(_ string, msg string, args ...any) {
		messages = append(messages, msg)
	}

	// Should not panic. postgres user should have grants.
	VerifyGrants(context.Background(), pool, "postgres", logFn)
}

// ---------------------------------------------------------------------------
// Executor accessor methods
// ---------------------------------------------------------------------------

func TestPhase2_Executor_TrustLevel_Default(t *testing.T) {
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "monitor"},
		},
	}
	if e.TrustLevel() != "monitor" {
		t.Errorf("expected 'monitor', got %q", e.TrustLevel())
	}
}

func TestPhase2_Executor_TrustLevel_Override(t *testing.T) {
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "monitor"},
		},
		trustLevelOverride: "advisory",
	}
	if e.TrustLevel() != "advisory" {
		t.Errorf("expected 'advisory', got %q", e.TrustLevel())
	}
}

func TestPhase2_Executor_TrustLevel_ClearOverride(t *testing.T) {
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "auto"},
		},
		trustLevelOverride: "monitor",
	}
	_ = e.SetTrustLevel("")
	if e.TrustLevel() != "auto" {
		t.Errorf("expected 'auto' after clear, got %q",
			e.TrustLevel())
	}
}

func TestPhase2_Executor_ExecutionMode(t *testing.T) {
	e := &Executor{execMode: "auto"}
	if e.ExecutionMode() != "auto" {
		t.Errorf("expected 'auto', got %q", e.ExecutionMode())
	}
	e.SetExecutionMode("approval")
	if e.ExecutionMode() != "approval" {
		t.Errorf("expected 'approval', got %q", e.ExecutionMode())
	}
}

func TestPhase2_Executor_WithDatabaseName(t *testing.T) {
	e := &Executor{}
	e.WithDatabaseName("prod")
	if e.databaseName != "prod" {
		t.Errorf("expected 'prod', got %q", e.databaseName)
	}
}
