package safetybench

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "safety-bench"))
}

// newPool opens a pool on the designated disposable test database, skipping
// when no live server is configured. It holds the Supabase roles lock for
// the test, after the agent roles lock (the posture fixtures and the
// agent_query design create agent-named roles; the lock order is agent
// roles, then Supabase roles).
func newPool(ctx context.Context, t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	release, err := testdb.LockCluster(ctx, dsn, testdb.AgentRolesLock)
	if err != nil {
		t.Fatalf("agent roles lock: %v", err)
	}
	t.Cleanup(release)
	// The posture fixtures create the cluster-wide Supabase roles.
	testdb.HoldSupabaseRoles(t, dsn)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestAgentSafetyBench runs the full v0 bench and writes the report. It is
// gated by SAGE_SAFETY_BENCH_RUN=1 so the ./... suites stay within their
// timeouts; CI runs it in its own step. It fails only on bench machinery
// errors and on a self-check case that was not refused (a harness fault):
// measured numbers for the authored corpus are reported, not hard-failed.
func TestAgentSafetyBench(t *testing.T) {
	if !BenchRunRequested(os.Getenv) {
		t.Skipf("set %s=1 to run the full bench (CI runs it in its own step)", EnvRun)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := newPool(ctx, t)

	var version string
	if err := pool.QueryRow(ctx, "SELECT version()").Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	v, commit := BuildFromEnv(os.Getenv)
	report, err := Run(ctx, pool, Options{
		Posture: NewDetectorProvider(),
		Meta:    ReportMeta{ServerVersion: version, PgSageVersion: v, PgSageCommit: commit},
	})
	if err != nil {
		t.Fatalf("bench run: %v", err)
	}
	dir := ReportDir(os.Getenv(EnvReportDir), t.TempDir())
	jsonPath, mdPath, err := WriteReport(dir, report)
	if err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("report: %s, %s", jsonPath, mdPath)
	t.Log("\n" + report.Markdown())
	assertSelfChecksHeld(t, report)
}

// assertSelfChecksHeld fails if any self-check case was not refused by every
// design with checksums intact: that is a harness fault, not a corpus
// result.
func assertSelfChecksHeld(t *testing.T, r Report) {
	t.Helper()
	seen := false
	for _, c := range r.ReadOnly {
		if !isSelfCheck(c.ID) {
			continue
		}
		seen = true
		for _, a := range c.Attempts {
			if !a.Held() {
				t.Errorf("self-check %s not held by %s: observed=%s intact=%v detail=%q",
					c.ID, a.Design, a.Observed, a.ChecksumsIntact, a.Detail)
			}
		}
	}
	if !seen {
		t.Error("no self-check cases ran; the embedded fixtures are missing")
	}
}

func isSelfCheck(id string) bool {
	return len(id) >= 3 && id[:3] == "sc-"
}
