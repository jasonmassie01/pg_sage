package executor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rolegrants"
)

// ---------------------------------------------------------------------------
// connectTestDB helper
// ---------------------------------------------------------------------------

func connectTestDB2(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAGE_DATABASE_URL")
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

// ---------------------------------------------------------------------------
// checkSignalBackend probes the connected role: a superuser may signal, and
// a failed probe is logged and never reported as missing.
// ---------------------------------------------------------------------------

func TestPhase2_CheckSignalBackend_ConnectedRole(t *testing.T) {
	pool := connectTestDB2(t)
	defer pool.Close()
	var logged []string
	logFn := func(_ string, msg string, args ...any) {
		logged = append(logged, fmt.Sprintf(msg, args...))
	}
	var super bool
	if err := pool.QueryRow(context.Background(),
		"SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&super); err != nil {
		t.Fatalf("superuser: %v", err)
	}
	sig, lacking := checkSignalBackend(context.Background(), pool, logFn)
	if super && (lacking || !sig.Usable || sig.MemberOnly || sig.Version < 140000) {
		t.Fatalf("superuser: sig = %+v, lacking = %v; want usable", sig, lacking)
	}
	if len(logged) != 0 {
		t.Fatalf("logged %q, want nothing for a probe that ran", logged)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sig, lacking = checkSignalBackend(ctx, pool, logFn)
	if lacking || sig != (rolegrants.SignalBackend{}) || len(logged) != 1 ||
		!strings.Contains(logged[0], "could not check pg_signal_backend") {
		t.Fatalf("cancelled: sig = %+v, lacking = %v, logged %q", sig, lacking, logged)
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
// VerifyGrants full integration
// ---------------------------------------------------------------------------

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
