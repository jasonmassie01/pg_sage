package auditjob

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/auditjob"))
}

func fresh(t *testing.T, label string, bootstrap bool) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if bootstrap {
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func openFindings(t *testing.T, pool *pgxpool.Pool) (int, string, string) {
	t.Helper()
	var n int
	var sev, ident string
	err := pool.QueryRow(context.Background(), `SELECT count(*), coalesce(max(severity),''),
		coalesce(max(object_identifier),'') FROM sage.findings
		WHERE category = $1 AND status = 'open'`, FindingCategory).Scan(&n, &sev, &ident)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	return n, sev, ident
}

// An intact trail raises nothing; a tampered one raises one critical
// finding on the table; once the trail verifies again the finding
// resolves.
func TestVerificationFindingLifecycle(t *testing.T) {
	pool := fresh(t, "auditjob_lifecycle", true)
	ctx := context.Background()
	src := Source{Name: "orders", Pool: pool}
	mustExec(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('vacuum', 'VACUUM a'), ('vacuum', 'VACUUM b')`)
	reports, err := Run(ctx, src)
	if err != nil || len(reports) < 3 {
		t.Fatalf("intact run: %d reports, %v", len(reports), err)
	}
	if n, _, _ := openFindings(t, pool); n != 0 {
		t.Fatalf("intact trail raised %d findings", n)
	}
	mustExec(t, pool, "UPDATE sage.action_log SET sql_executed = 'SELECT 1' "+
		"WHERE sql_executed = 'VACUUM a'")
	if _, err := Run(ctx, src); err != nil {
		t.Fatalf("tampered run: %v", err)
	}
	n, sev, ident := openFindings(t, pool)
	if n != 1 || sev != "critical" || ident != "audit_trail:action_log" {
		t.Fatalf("tampered trail finding = %d %q %q", n, sev, ident)
	}
	if _, err := Run(ctx, src); err != nil {
		t.Fatalf("repeat run: %v", err)
	}
	if n, _, _ := openFindings(t, pool); n != 1 {
		t.Fatalf("repeat run opened %d findings, want the same one", n)
	}
	mustExec(t, pool, "UPDATE sage.action_log SET sql_executed = 'VACUUM a' "+
		"WHERE sql_executed = 'SELECT 1'")
	if _, err := Run(ctx, src); err != nil {
		t.Fatalf("restored run: %v", err)
	}
	if n, _, _ := openFindings(t, pool); n != 0 {
		t.Fatalf("restored trail still has %d open findings", n)
	}
}

// A database without the sage schema has no chains: nothing to verify,
// no error; a dead pool is an error.
func TestRunWithoutChainsAndOnErrors(t *testing.T) {
	bare := fresh(t, "auditjob_bare", false)
	reports, err := Run(context.Background(), Source{Name: "bare", Pool: bare})
	if err != nil || len(reports) != 0 {
		t.Fatalf("bare database: %d reports, %v", len(reports), err)
	}
	bare.Close()
	if _, err := Run(context.Background(), Source{Name: "bare", Pool: bare}); err == nil {
		t.Fatalf("closed pool: no error")
	}
	if _, err := Run(context.Background(), Source{Name: "nil"}); err == nil {
		t.Fatalf("nil pool: no error")
	}
}
