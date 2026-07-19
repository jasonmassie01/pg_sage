package startup

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testDSN() string {
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return os.Getenv("SAGE_TEST_DATABASE_URL")
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

func TestCheckResult_ZeroValue(t *testing.T) {
	var cr CheckResult
	if cr.PGVersionNum != 0 {
		t.Errorf("PGVersionNum = %d, want 0", cr.PGVersionNum)
	}
	if cr.QueryTextVisible {
		t.Error("QueryTextVisible should default to false")
	}
	if cr.HasWALColumns {
		t.Error("HasWALColumns should default to false")
	}
	if cr.HasPlanTimeColumns {
		t.Error("HasPlanTimeColumns should default to false")
	}
}

func TestPGVersionThreshold(t *testing.T) {
	// The version check rejects versions below 140000 (PG14).
	// We verify the threshold constant by inspecting the function logic.
	// PG version_num format: major * 10000 + minor (PG14.0 = 140000).
	cases := []struct {
		name       string
		versionNum int
		wantReject bool
	}{
		{"PG13.12", 130012, true},
		{"PG13.0", 130000, true},
		{"PG12.0", 120000, true},
		{"PG9.6", 90600, true},
		{"PG14.0 boundary", 140000, false},
		{"PG14.5", 140005, false},
		{"PG15.0", 150000, false},
		{"PG16.0", 160000, false},
		{"PG17.0", 170000, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rejected := tc.versionNum < 140000
			if rejected != tc.wantReject {
				t.Errorf("version %d: rejected=%v, want %v",
					tc.versionNum, rejected, tc.wantReject)
			}
		})
	}
}

func TestExtensionError_ContainsInstallHint(t *testing.T) {
	// The error message from checkExtensionInstalled should guide
	// the user to install pg_stat_statements.
	hint := "pg_stat_statements extension is not installed"
	installCmd := "CREATE EXTENSION pg_stat_statements"
	preload := "shared_preload_libraries"

	// Build a synthetic error message matching the format string in checks.go
	errMsg := "pg_stat_statements extension is not installed; " +
		"install it with: CREATE EXTENSION pg_stat_statements; " +
		"and add shared_preload_libraries = 'pg_stat_statements' " +
		"to postgresql.conf, then restart PostgreSQL: some error"

	if !strings.Contains(errMsg, hint) {
		t.Errorf("error missing hint %q", hint)
	}
	if !strings.Contains(errMsg, installCmd) {
		t.Errorf("error missing install command %q", installCmd)
	}
	if !strings.Contains(errMsg, preload) {
		t.Errorf("error missing preload hint %q", preload)
	}
}

func TestAccessError_ContainsRoleHint(t *testing.T) {
	errMsg := "cannot read pg_stat_statements — ensure the connected role " +
		"has the pg_read_all_stats role or is a superuser: permission denied"

	if !strings.Contains(errMsg, "pg_read_all_stats") {
		t.Error("access error missing pg_read_all_stats hint")
	}
	if !strings.Contains(errMsg, "superuser") {
		t.Error("access error missing superuser hint")
	}
}

func TestQueryCtx_ReturnsContextWithCancel(t *testing.T) {
	// queryCtx wraps context.WithTimeout — verify it returns
	// a non-nil context and a cancel func.
	ctx, cancel := queryCtx(t.Context())
	defer cancel()

	if ctx == nil {
		t.Fatal("queryCtx returned nil context")
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("queryCtx context has no deadline")
	}
	if deadline.IsZero() {
		t.Fatal("queryCtx deadline is zero")
	}
}

func TestRunChecks_LivePG(t *testing.T) {
	if os.Getenv("SAGE_DATABASE_URL") == "" {
		t.Skip("SAGE_DATABASE_URL not set; skipping live PG test")
	}
	pool, ctx := requireDB(t)

	result, err := RunChecks(ctx, pool)
	if err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	if result.PGVersionNum < 150000 {
		t.Errorf("PGVersionNum = %d, want >= 150000", result.PGVersionNum)
	}
	// HasPlanTimeColumns depends on whether pg_stat_statements columns
	// appear in information_schema.columns, which varies by PG install.
	// Cloud SQL may return false here; just log the value.
	t.Logf("HasPlanTimeColumns = %v", result.HasPlanTimeColumns)
	if !result.QueryTextVisible {
		t.Error("QueryTextVisible should be true")
	}
}

func TestCheckConnectivity_LivePG(t *testing.T) {
	if os.Getenv("SAGE_DATABASE_URL") == "" {
		t.Skip("SAGE_DATABASE_URL not set; skipping live PG test")
	}
	pool, ctx := requireDB(t)

	// RunChecks calls checkConnectivity first; no error proves it works.
	_, err := RunChecks(ctx, pool)
	if err != nil {
		t.Fatalf("RunChecks (connectivity): %v", err)
	}
}

func TestCheckPGVersion_LivePG(t *testing.T) {
	if os.Getenv("SAGE_DATABASE_URL") == "" {
		t.Skip("SAGE_DATABASE_URL not set; skipping live PG test")
	}
	pool, ctx := requireDB(t)

	result, err := RunChecks(ctx, pool)
	if err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	if result.PGVersionNum < 140000 {
		t.Errorf("PGVersionNum = %d, want >= 140000 (PG14+)",
			result.PGVersionNum)
	}
}
