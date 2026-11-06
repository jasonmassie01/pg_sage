package safetybench

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/sqlast"
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
	testdb.LockAgentRolesOn(t, dsn)
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
// timeouts; CI runs it in its own step. It fails on bench machinery errors,
// on a self-check that some design did not hold with a credited refusal (a
// harness fault, SelfCheckFailures), on a case the canonical design refused
// by another credited class than it declares (the case drifted), and on any
// cell that differs from testdata/baseline.json (checkBaseline).
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
	if report.SQLParser != sqlast.Available() {
		t.Errorf("report sql_parser = %v, want %v (this build)", report.SQLParser,
			sqlast.Available())
	}
	dir := ReportDir(os.Getenv(EnvReportDir), t.TempDir())
	jsonPath, mdPath, err := WriteReport(dir, report)
	if err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("report: %s, %s", jsonPath, mdPath)
	t.Log("\n" + report.Markdown())
	for _, f := range SelfCheckFailures(report) {
		t.Error(f)
	}
	for _, m := range report.ExpectMismatches() {
		if m.Fails() {
			t.Errorf("%s declares %s but %s observed %s", m.ID, m.Expect, canonicalDesign,
				m.Observed)
		}
	}
	checkBaseline(t, report, jsonPath)
}

// checkBaseline compares every cell with the committed baseline: a
// regression fails naming the design and case, and so does an improvement
// (regenerate the baseline so the published numbers stay true).
func checkBaseline(t *testing.T, report Report, jsonPath string) {
	t.Helper()
	if !report.SQLParser && ParserRequired(os.Getenv) {
		t.Fatalf("CI build lacks the libpg_query parser (CGO_ENABLED=0): the explain_guard "+
			"and agent_query cells cannot be compared with %s; build with cgo", BaselineFile)
	}
	b, err := LoadBaseline(BaselineFile)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := DiffBaseline(report, b)
	if err != nil {
		t.Fatalf("compare with %s: %v", BaselineFile, err)
	}
	if len(diff.NotCompared) > 0 {
		t.Logf("%d cells not compared with %s: this build lacks the libpg_query parser "+
			"(CGO_ENABLED=0), so explain_guard and agent_query refuse every statement: %s",
			len(diff.NotCompared), BaselineFile, strings.Join(diff.NotCompared, ", "))
	}
	for _, line := range diff.Failures() {
		t.Errorf("%s (report %s)", line, jsonPath)
	}
}
