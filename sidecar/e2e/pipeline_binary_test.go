//go:build e2e

// Layer A of the pipeline-coverage suite: the REAL sidecar binary runs
// against the throwaway Postgres with autonomous trust, and every
// deterministically-seedable Tier-1 rule must travel detection → finding →
// trust gate → executed action → observable side effect, unaided.
//
// LLM-driven categories (optimizer/advisor/tuner) are excluded here — the
// binary runs with llm.enabled=false — their executor tails are covered
// shape-by-shape in Layer B.
package e2e

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	pipelineBinaryDeadline = 180 * time.Second
	pipelinePollEvery      = 3 * time.Second
)

func TestPipelineCoverage_FullBinary(t *testing.T) {
	dsn := pipelineURL(t)
	// No Bootstrap here: the binary must bootstrap the sage schema itself
	// (and holds the bootstrap advisory lock while running).
	pool := rawPipelinePool(t)
	report := &checkReport{}
	defer report.dump(t, "Layer A — full-binary detection→action checks")

	seedBinaryScenarios(t, pool, dsn)

	binPath := buildBinary(t)
	cfgPath := writePipelineConfig(t, dsn)
	stopBinary := startPipelineBinary(t, binPath, cfgPath, dsn)
	defer stopBinary()

	waitForBinaryCycles(t, pool)
	assertBinaryExpectations(t, pool, report)
}

// --- seeding ----------------------------------------------------------------

func seedBinaryScenarios(t *testing.T, pool *pgxpool.Pool, dsn string) {
	t.Helper()
	mustExec(t, pool, `
		DROP TABLE IF EXISTS pipe_dup, pipe_child, pipe_parent,
			pipe_bloat, pipe_stale, pipe_av, pipe_invalid CASCADE;
		DROP SEQUENCE IF EXISTS pipe_seq;`)

	// A1 duplicate_index → executor drops one copy.
	mustExec(t, pool, `
		CREATE TABLE pipe_dup (s text);
		INSERT INTO pipe_dup SELECT g::text FROM generate_series(1,2000) g;
		CREATE INDEX pipe_dup_a ON pipe_dup (s);
		CREATE INDEX pipe_dup_b ON pipe_dup (s);`)

	// A2 missing_fk_index → executor creates the FK index.
	mustExec(t, pool, `
		CREATE TABLE pipe_parent (id bigint PRIMARY KEY);
		INSERT INTO pipe_parent SELECT g FROM generate_series(1,2000) g;
		CREATE TABLE pipe_child (
			id bigserial PRIMARY KEY,
			parent_id bigint NOT NULL REFERENCES pipe_parent (id)
		);
		INSERT INTO pipe_child (parent_id)
		SELECT 1 + g % 2000 FROM generate_series(1,20000) g;`)

	// A3 table_bloat → executor vacuums. autovacuum off so the daemon
	// cannot steal the fix before pg_sage does.
	mustExec(t, pool, `
		CREATE TABLE pipe_bloat (id int) WITH (autovacuum_enabled = false);
		INSERT INTO pipe_bloat SELECT g FROM generate_series(1,20000) g;
		DELETE FROM pipe_bloat WHERE id % 2 = 0;`)

	// A4 stale_statistics → executor analyzes (never analyzed + writes).
	mustExec(t, pool, `
		CREATE TABLE pipe_stale (id int) WITH (autovacuum_enabled = false);
		INSERT INTO pipe_stale SELECT g FROM generate_series(1,20000) g;`)

	// A5 autovacuum_tuning → executor sets per-table scale factor.
	// Trigger: live >= autovacuum_tune_min_rows AND upd+del >= live.
	mustExec(t, pool, `
		CREATE TABLE pipe_av (id int, n int) WITH (autovacuum_enabled = false);
		INSERT INTO pipe_av SELECT g, 0 FROM generate_series(1,6000) g;
		UPDATE pipe_av SET n = 1;
		UPDATE pipe_av SET n = 2;`)

	// A6 sequence_exhaustion → advisory finding, NO action.
	mustExec(t, pool, `
		CREATE SEQUENCE pipe_seq AS smallint;
		SELECT setval('pipe_seq', 26000);`)

	// A7 slow_query → advisory finding, NO action. Needs
	// pg_stat_statements (preloaded by the runner's container).
	mustExec(t, pool, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements;`)
	for i := 0; i < 3; i++ {
		mustExec(t, pool, "SELECT pg_sleep(0.8)")
	}

	// A8 invalid_index → executor drops the invalid leftover. A tiny
	// statement_timeout aborts a CONCURRENTLY build, which by design
	// leaves an INVALID index behind.
	mustExec(t, pool, `
		CREATE TABLE pipe_invalid (id int);
		INSERT INTO pipe_invalid SELECT g FROM generate_series(1,300000) g;`)
	seedInvalidIndex(t, dsn)
}

// seedInvalidIndex aborts a CONCURRENTLY build to leave an invalid index.
func seedInvalidIndex(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("invalid-index conn: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "SET statement_timeout = '15ms'"); err != nil {
		t.Fatalf("set statement_timeout: %v", err)
	}
	_, err = conn.Exec(ctx,
		"CREATE INDEX CONCURRENTLY pipe_invalid_idx ON pipe_invalid (id)")
	if err == nil {
		t.Fatalf("expected the CONCURRENTLY build to time out and leave " +
			"an invalid index; it succeeded — raise the row count")
	}
	var valid bool
	err = conn.QueryRow(ctx,
		`SELECT i.indisvalid FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relname = 'pipe_invalid_idx'`).Scan(&valid)
	if err != nil || valid {
		t.Fatalf("invalid-index seed: exists-and-invalid check failed "+
			"(err=%v valid=%v)", err, valid)
	}
}

// --- binary lifecycle --------------------------------------------------------

// writePipelineConfig renders a standalone config aimed at the pipeline DB
// with autonomous trust, fast cycles, low rule thresholds, and no LLM.
func writePipelineConfig(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse PIPELINE_PG_URL: %v", err)
	}
	pass, _ := u.User.Password()
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "5432"
	}
	db := strings.TrimPrefix(u.Path, "/")

	apiPort := freePort(t)
	promPort := freePort(t)

	yaml := fmt.Sprintf(`mode: standalone

postgres:
  host: %s
  port: %s
  user: %s
  password: %s
  database: %s
  sslmode: disable
  max_connections: 4

collector:
  interval_seconds: 3

analyzer:
  interval_seconds: 6
  slow_query_threshold_ms: 500
  seq_scan_min_rows: 1000000
  unused_index_window_days: 7
  table_bloat_dead_tuple_pct: 10
  table_bloat_min_rows: 1000
  autovacuum_tune_min_rows: 5000
  analyze_stale_min_rows: 1000

safety:
  query_timeout_ms: 5000
  lock_timeout_ms: 5000
  ddl_timeout_seconds: 60

trust:
  level: autonomous
  ramp_start: "2026-01-01T00:00:00Z"
  maintenance_window: "always"
  tier3_safe: true
  tier3_moderate: true
  tier3_high_risk: false

llm:
  enabled: false

prometheus:
  listen_addr: "127.0.0.1:%d"

api:
  listen_addr: "127.0.0.1:%d"
`, host, port, u.User.Username(), pass, db, promPort, apiPort)

	path := filepath.Join(t.TempDir(), "pipeline-config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write pipeline config: %v", err)
	}
	return path
}

func startPipelineBinary(
	t *testing.T, binPath, cfgPath, dsn string,
) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binPath, "--config", cfgPath)
	cmd.Env = append(os.Environ(),
		"SAGE_DATABASE_URL="+dsn,
		"SAGE_MODE=standalone",
		"SAGE_LLM_API_KEY=",
		"SAGE_META_DB=",
		"LLM_API_KEY=",
	)
	out := &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start pipeline binary: %v", err)
	}
	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			tail := out.String()
			if len(tail) > 8000 {
				tail = tail[len(tail)-8000:]
			}
			t.Logf("--- binary output tail ---\n%s", tail)
		}
	}
}

// waitForBinaryCycles waits until the binary has produced its first finding
// (proof the collector+analyzer loop is alive), then allows several more
// cycles for the executor to act.
func waitForBinaryCycles(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(pipelineBinaryDeadline)
	for time.Now().Before(deadline) {
		// quickScalar tolerates "relation sage.findings does not exist"
		// while the binary is still bootstrapping its schema.
		n := quickScalar(pool, "SELECT count(*)::text FROM sage.findings")
		if n != "0" && !strings.HasPrefix(n, "<err") {
			return
		}
		time.Sleep(pipelinePollEvery)
	}
	t.Fatalf("binary produced no findings within %s", pipelineBinaryDeadline)
}

// --- expectations -------------------------------------------------------------

type binaryExpectation struct {
	id       string
	desc     string
	check    func(pool *pgxpool.Pool) (bool, string)
	terminal bool // once true it cannot regress; stop polling it
}

func assertBinaryExpectations(
	t *testing.T, pool *pgxpool.Pool, report *checkReport,
) {
	t.Helper()
	expectations := []binaryExpectation{
		{
			id:   "CHECK-A01",
			desc: "duplicate_index: one of pipe_dup_a/b auto-dropped, one intact",
			check: func(p *pgxpool.Pool) (bool, string) {
				n := quickScalar(p, `SELECT count(*)::text FROM pg_indexes
					WHERE indexname IN ('pipe_dup_a','pipe_dup_b')`)
				act := quickScalar(p, `SELECT count(*)::text FROM sage.action_log
					WHERE action_type = 'drop_index'
					  AND sql_executed LIKE '%pipe_dup_%'
					  AND outcome IN ('success','monitoring')`)
				return n == "1" && act != "0",
					fmt.Sprintf("indexes_left=%s drop_actions=%s", n, act)
			},
		},
		{
			id:   "CHECK-A02",
			desc: "missing_fk_index: index auto-created on pipe_child(parent_id)",
			check: func(p *pgxpool.Pool) (bool, string) {
				n := quickScalar(p, `SELECT count(*)::text FROM pg_indexes
					WHERE tablename = 'pipe_child'
					  AND indexdef LIKE '%(parent_id)%'`)
				act := quickScalar(p, `SELECT count(*)::text FROM sage.action_log
					WHERE action_type = 'create_index'
					  AND sql_executed LIKE '%pipe_child%'
					  AND outcome IN ('success','monitoring')`)
				return n != "0" && act != "0",
					fmt.Sprintf("fk_indexes=%s create_actions=%s", n, act)
			},
		},
		{
			id:   "CHECK-A03",
			desc: "table_bloat: pipe_bloat auto-vacuumed by pg_sage",
			check: func(p *pgxpool.Pool) (bool, string) {
				lv := quickScalar(p, `SELECT coalesce(last_vacuum::text,'')
					FROM pg_stat_user_tables WHERE relname = 'pipe_bloat'`)
				act := quickScalar(p, `SELECT count(*)::text FROM sage.action_log
					WHERE action_type = 'vacuum'
					  AND sql_executed LIKE '%pipe_bloat%'
					  AND outcome IN ('success','monitoring')`)
				return lv != "" && act != "0",
					fmt.Sprintf("last_vacuum=%q vacuum_actions=%s", lv, act)
			},
		},
		{
			id:   "CHECK-A04",
			desc: "stale_statistics: pipe_stale auto-analyzed by pg_sage",
			check: func(p *pgxpool.Pool) (bool, string) {
				la := quickScalar(p, `SELECT coalesce(last_analyze::text,'')
					FROM pg_stat_user_tables WHERE relname = 'pipe_stale'`)
				act := quickScalar(p, `SELECT count(*)::text FROM sage.action_log
					WHERE action_type = 'analyze'
					  AND sql_executed LIKE '%pipe_stale%'
					  AND outcome IN ('success','monitoring')`)
				return la != "" && act != "0",
					fmt.Sprintf("last_analyze=%q analyze_actions=%s", la, act)
			},
		},
		{
			id:   "CHECK-A05",
			desc: "autovacuum_tuning: per-table scale factor applied to pipe_av",
			check: func(p *pgxpool.Pool) (bool, string) {
				opts := quickScalar(p, `SELECT
					coalesce(array_to_string(reloptions, ','), '')
					FROM pg_class WHERE relname = 'pipe_av'`)
				return strings.Contains(opts, "autovacuum_vacuum_scale_factor"),
					fmt.Sprintf("reloptions=%q", opts)
			},
		},
		{
			id:   "CHECK-A06",
			desc: "sequence_exhaustion: advisory finding exists, ZERO actions",
			check: func(p *pgxpool.Pool) (bool, string) {
				f := quickScalar(p, `SELECT count(*)::text FROM sage.findings
					WHERE category = 'sequence_exhaustion'
					  AND object_identifier LIKE '%pipe_seq%'`)
				a := quickScalar(p, `SELECT count(*)::text FROM sage.action_log
					WHERE sql_executed LIKE '%pipe_seq%'`)
				return f != "0" && a == "0",
					fmt.Sprintf("findings=%s actions=%s", f, a)
			},
		},
		{
			id:   "CHECK-A07",
			desc: "slow_query: advisory finding exists for pg_sleep, ZERO actions",
			check: func(p *pgxpool.Pool) (bool, string) {
				f := quickScalar(p, `SELECT count(*)::text FROM sage.findings
					WHERE category = 'slow_query'`)
				a := quickScalar(p, `SELECT count(*)::text FROM sage.action_log
					WHERE sql_executed LIKE '%pg_sleep%'`)
				return f != "0" && a == "0",
					fmt.Sprintf("findings=%s actions=%s", f, a)
			},
		},
		{
			id:   "CHECK-A08",
			desc: "invalid_index: pipe_invalid_idx auto-dropped",
			check: func(p *pgxpool.Pool) (bool, string) {
				n := quickScalar(p, `SELECT count(*)::text FROM pg_indexes
					WHERE indexname = 'pipe_invalid_idx'`)
				return n == "0", fmt.Sprintf("invalid_index_left=%s", n)
			},
		},
		{
			id:   "CHECK-A09",
			desc: "no failed actions anywhere in the run",
			check: func(p *pgxpool.Pool) (bool, string) {
				n := quickScalar(p, `SELECT count(*)::text FROM sage.action_log
					WHERE outcome = 'failed'`)
				detail := quickScalar(p, `SELECT coalesce(string_agg(
					left(sql_executed, 40) || ' → ' ||
					coalesce(rollback_reason,''), ' | '), '')
					FROM sage.action_log WHERE outcome = 'failed'`)
				return n == "0", fmt.Sprintf("failed=%s %s", n, detail)
			},
		},
	}

	deadline := time.Now().Add(pipelineBinaryDeadline)
	passed := make(map[string]string, len(expectations))
	for time.Now().Before(deadline) && len(passed) < len(expectations) {
		for _, e := range expectations {
			if _, done := passed[e.id]; done {
				continue
			}
			if ok, detail := e.check(pool); ok {
				passed[e.id] = detail
			}
		}
		if len(passed) < len(expectations) {
			time.Sleep(pipelinePollEvery)
		}
	}

	for _, e := range expectations {
		detail, ok := passed[e.id]
		if !ok {
			_, detail = e.check(pool)
		}
		report.add(t, e.id, ok, e.desc+" — "+detail)
	}
}

// quickScalar is scalarString without *testing.T (used inside poll closures).
func quickScalar(pool *pgxpool.Pool, sql string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var v string
	if err := pool.QueryRow(ctx, sql).Scan(&v); err != nil {
		return "<err: " + err.Error() + ">"
	}
	return v
}
