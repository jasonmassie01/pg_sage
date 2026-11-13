package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/verify"
)

// market-08b on a live server: an enforced 50 ms statement_timeout cancels
// real pg_sleep(0.1) statements; the counter counts the server's 57014
// reports into sage.budget_shadow; the monitor judges the action against
// its shadow prediction, reverts it through the rollback path under the
// role lock and pg_db_role_setting is byte-identical to before. The budget
// then stays in warn mode.

const revertBudget = "exec-app-50ms"

// timeoutRevertFixture is an enforced 50 ms timeout recorded as a monitored
// action executed back before now.
type timeoutRevertFixture struct {
	pool     *pgxpool.Pool
	e        *Executor
	db       string
	before   string
	sql      string
	rollback string
	actionID int64
	store    *budget.Store
}

func enforceRevertFixture(t *testing.T, back time.Duration, perHour float64) timeoutRevertFixture {
	t.Helper()
	p, ctx := requireDB(t)
	db := budgetRoleSetup(t, p)
	quoted := pgx.Identifier{db}.Sanitize()
	if _, err := p.Exec(ctx, `ALTER ROLE `+budgetRole+` IN DATABASE `+quoted+
		` SET statement_timeout = '45s'`); err != nil {
		t.Fatal(err)
	}
	f := timeoutRevertFixture{pool: p, e: roleBudgetExecutor(p), db: db, before: roleSettings(t, p),
		sql:   budget.RoleSettingSQL(budgetRole, db, budget.SettingStatementTimeout, 50),
		store: budget.NewStore(p, "revert-fleet")}
	change, err := runRoleBudget(t, f.e, f.sql)
	if err != nil {
		t.Fatalf("enforce: %v", err)
	}
	f.rollback = change.rollbackSQL
	state := map[string]any{}
	change.record(state)
	state["predicted_effect"] = verify.Prediction{Class: verify.ClassRoleTimeout,
		Method: verify.MethodRule, Metric: verify.MetricStatementTimeoutCancels,
		ExpectedChangePct: new(float64), Baseline: &perHour, Source: "budget"}
	state["cancel_watch"] = map[string]any{"budget": revertBudget,
		"ledger_database": "revert-fleet", "role": budgetRole, "predicted_per_hour": perHour,
		"actual_at_apply": 0, "source": budget.CancelSourceLog}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	err = p.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		executed_at, outcome, rollback_sql, before_state)
		VALUES ('set_role_timeout', $1, now() - $2::interval, 'monitoring', $3, $4)
		RETURNING id`, f.sql, back.String(), f.rollback, raw).Scan(&f.actionID)
	if err != nil {
		t.Fatalf("record the action: %v", err)
	}
	t.Cleanup(func() { forgetRevertAction(p, f.actionID) })
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if err := f.store.Record(ctx, today, []budget.Tally{{Budget: revertBudget,
		Definition: "role=" + budgetRole}}); err != nil {
		t.Fatal(err)
	}
	return f
}

// realCancels runs pg_sleep(0.1) n times as the budgeted role under its
// enforced 50 ms timeout and returns the server's error reports as log
// entries.
func realCancels(t *testing.T, f timeoutRevertFixture, n int) []logwatch.LogEntry {
	t.Helper()
	ctx := context.Background()
	cfg := f.pool.Config().ConnConfig.Copy()
	cfg.User, cfg.Password, cfg.Database = budgetRole, "budget-test", f.db
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", budgetRole, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var out []logwatch.LogEntry
	for range n {
		_, err := conn.Exec(ctx, "SELECT pg_sleep(0.1)")
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
			t.Fatalf("pg_sleep(0.1) under a 50 ms timeout: %v, want SQLSTATE 57014", err)
		}
		out = append(out, logwatch.LogEntry{Timestamp: time.Now().UTC(), User: budgetRole,
			Database: f.db, ErrorLevel: pgErr.Severity, SQLState: pgErr.Code,
			Message: pgErr.Message})
	}
	return out
}

type entrySlice struct{ entries []logwatch.LogEntry }

func (s *entrySlice) Drain() []logwatch.LogEntry {
	out := s.entries
	s.entries = nil
	return out
}

func (s *entrySlice) TakeEvicted() int { return 0 }

// countCancels feeds entries through the budget's counter into the ledger.
func countCancels(t *testing.T, f timeoutRevertFixture, src budget.EntrySource) {
	t.Helper()
	c := budget.NewCancelCounter(f.db, src, func() bool { return true })
	c.Poll(time.Now())
	batch := c.Take()
	var rows []budget.CancelRow
	for _, rc := range batch.Counts {
		rows = append(rows, budget.CancelRow{Budget: revertBudget,
			Definition: "role=" + budgetRole, Day: rc.Day, Count: rc.Count})
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if err := f.store.RecordCancels(context.Background(), today, budget.CancelSourceLog,
		rows); err != nil {
		t.Fatalf("record cancels: %v", err)
	}
}

func revertMonitorConfig(f timeoutRevertFixture) RollbackMonitorConfig {
	return f.e.rollbackMonitorConfig(func(context.Context, string) bool { return true })
}

func timeoutActionOutcome(t *testing.T, p *pgxpool.Pool, id int64) (string, string) {
	t.Helper()
	var outcome, reason string
	if err := p.QueryRow(context.Background(), `SELECT outcome, COALESCE(rollback_reason, '')
		FROM sage.action_log WHERE id = $1`, id).Scan(&outcome, &reason); err != nil {
		t.Fatal(err)
	}
	return outcome, reason
}

// Integration and happy path: 30 real cancels in 3 h against a 2/h
// prediction revert the timeout; the verdict and its evidence carry the
// numbers; the role's settings are byte-identical to before.
func TestRoleBudgetRevert_RealCancelsRevert(t *testing.T) {
	f := enforceRevertFixture(t, 3*time.Hour, 2)
	countCancels(t, f, &entrySlice{entries: realCancels(t, f, 30)})
	MonitorAndRollback(context.Background(), f.pool, f.actionID, f.rollback,
		revertMonitorConfig(f), nopLog, nil)
	if outcome, reason := timeoutActionOutcome(t, f.pool, f.actionID); outcome != "rolled_back" {
		t.Fatalf("outcome = %s (%s), want rolled_back", outcome, reason)
	}
	if after := roleSettings(t, f.pool); after != f.before {
		t.Fatalf("pg_db_role_setting after the revert = %q, before = %q", after, f.before)
	}
	var verdict, reason string
	var evidence []byte
	err := f.pool.QueryRow(context.Background(), `SELECT verdict, reason, evidence::text
		FROM sage.action_outcome WHERE action_log_id = $1`, f.actionID).
		Scan(&verdict, &reason, &evidence)
	if err != nil {
		t.Fatalf("read the verdict: %v", err)
	}
	if verdict != verify.OutcomeRegressed || !strings.Contains(reason, "30 cancels in 3.0 h") ||
		!strings.Contains(reason, "prediction 6.0") {
		t.Fatalf("verdict = %s (%s), want regressed naming 30 cancels and prediction 6.0",
			verdict, reason)
	}
	if !strings.Contains(string(evidence), `"actual_cancels": 30`) &&
		!strings.Contains(string(evidence), `"actual_cancels":30`) {
		t.Fatalf("evidence lacks the cancel count: %s", evidence)
	}
	assertNotReEnforced(t, f)
}

// Boundary: 11 cancels in 3 h against 2/h is within 2x; the action keeps
// being watched (insufficient, accruing) and nothing is rolled back.
func TestRoleBudgetRevert_WithinTwiceThePredictionKeeps(t *testing.T) {
	f := enforceRevertFixture(t, 3*time.Hour, 2)
	countCancels(t, f, &entrySlice{entries: realCancels(t, f, 11)})
	cfg := revertMonitorConfig(f)
	plan, err := loadMonitorPlan(context.Background(), f.pool, f.actionID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if plan.capWindow < 24*time.Hour || plan.minWindow != 15*time.Minute {
		t.Fatalf("windows = min %s cap %s, want 15m and at least 24h", plan.minWindow,
			plan.capWindow)
	}
	j := plan.judge(context.Background(), f.pool, cfg, time.Now())
	if j.outcome.Verdict != verify.OutcomeInsufficient || !j.accruing {
		t.Fatalf("verdict = %s (%s) accruing %v, want insufficient and accruing",
			j.outcome.Verdict, j.outcome.Reason, j.accruing)
	}
	if roleSettings(t, f.pool) == f.before {
		t.Fatal("the enforced setting is gone")
	}
}

// Error propagation: a rollback refused for lack of privilege (42501)
// leaves rollback_failed with the server's message, and the budget is not
// re-enforced by the next cycle.
func TestRoleBudgetRevert_PermissionDeniedRollbackFails(t *testing.T) {
	f := enforceRevertFixture(t, 3*time.Hour, 2)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `DROP ROLE IF EXISTS budget_exec_norevert`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `CREATE ROLE budget_exec_norevert LOGIN
		PASSWORD 'norevert-test'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DROP ROLE budget_exec_norevert`) })
	pc := f.pool.Config().Copy()
	pc.ConnConfig.User, pc.ConnConfig.Password = "budget_exec_norevert", "norevert-test"
	limited, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	defer limited.Close()
	countCancels(t, f, &entrySlice{entries: realCancels(t, f, 30)})
	cfg := revertMonitorConfig(f)
	cfg.ActionPool = limited
	MonitorAndRollback(ctx, f.pool, f.actionID, f.rollback, cfg, nopLog, nil)
	outcome, reason := timeoutActionOutcome(t, f.pool, f.actionID)
	if outcome != "rollback_failed" || !strings.Contains(reason, "permission denied") {
		t.Fatalf("outcome = %s (%s), want rollback_failed with the server message",
			outcome, reason)
	}
	if roleSettings(t, f.pool) == f.before {
		t.Fatal("a failed rollback changed the role's settings")
	}
	assertNotReEnforced(t, f)
}

// assertNotReEnforced runs the budget monitor on the live ledger with 8
// clean days of shadow data: the reverted (or failed) budget raises no
// statement_timeout action.
func assertNotReEnforced(t *testing.T, f timeoutRevertFixture) {
	t.Helper()
	ctx := context.Background()
	cfg := config.DefaultConfig()
	cfg.Budgets = []config.BudgetConfig{{Name: revertBudget, Match: config.BudgetMatch{
		Role: budgetRole}, StatementTimeout: "50ms", Mode: "enforce"}}
	b, err := budget.FromConfig(cfg.Budgets)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for back := 8; back >= 0; back-- {
		d := now.Add(-time.Duration(back) * 24 * time.Hour).Truncate(24 * time.Hour)
		if err := f.store.Record(ctx, d, []budget.Tally{{Budget: revertBudget,
			Definition: b[0].Definition()}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(ctx, `UPDATE sage.budget_shadow SET first_seen = day::timestamptz
		WHERE database_name = 'revert-fleet'`); err != nil {
		t.Fatal(err)
	}
	snap := &collector.Snapshot{CollectedAt: now, WorkloadSessions: &collector.WorkloadSessions{}}
	m := budget.NewMonitor(cfg, staticSnapshot{snap}, f.store,
		budget.NewCatalog(f.pool, catalogread.Default()), "revert-fleet", nopLog)
	fs, err := m.Detect(ctx)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	for _, finding := range fs {
		if finding.Category == budget.CategoryEnforce &&
			finding.Detail["setting"] == budget.SettingStatementTimeout {
			t.Fatalf("the budget was re-enforced after its revert: %s",
				finding.RecommendedSQL)
		}
	}
}

type staticSnapshot struct{ snap *collector.Snapshot }

func (s staticSnapshot) LatestSnapshot() *collector.Snapshot { return s.snap }

// csvlogDirEnv names the locally mounted csvlog directory of the
// SAGE_TEST_DATABASE_URL server.
const csvlogDirEnv = "SAGE_TEST_SERVER_CSVLOG_DIR"

// Integration with the server's own log: the same revert, with the cancels
// read by logwatch from the server's csvlog file.
func TestRoleBudgetRevert_ServerCSVLog(t *testing.T) {
	dir := os.Getenv(csvlogDirEnv)
	if dir == "" {
		t.Skip(csvlogDirEnv + " not set: needs the locally mounted csvlog directory " +
			"(log_destination = csvlog, logging_collector = on) of the " +
			"SAGE_TEST_DATABASE_URL server")
	}
	f := enforceRevertFixture(t, 3*time.Hour, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fw := logwatch.NewFileWatcher(config.LogWatchConfig{LogDirectory: dir, Format: "csvlog",
		PollIntervalMs: 200, MaxLineLenBytes: 65536}, nopLog)
	if err := fw.Start(ctx); err != nil {
		t.Fatalf("start watcher on %s: %v", dir, err)
	}
	defer fw.Stop()
	sub := fw.SubscribeEntries("budget:revert", f.db)
	fw.Drain()
	sub.Drain() // the backlog before this test
	realCancels(t, f, 30)
	got := &entrySlice{}
	for ctx.Err() == nil && countTimeouts(got.entries) < 30 {
		time.Sleep(300 * time.Millisecond)
		fw.Drain()
		got.entries = append(got.entries, sub.Drain()...)
	}
	countCancels(t, f, got)
	MonitorAndRollback(ctx, f.pool, f.actionID, f.rollback, revertMonitorConfig(f), nopLog,
		nil)
	if outcome, reason := timeoutActionOutcome(t, f.pool, f.actionID); outcome != "rolled_back" {
		t.Fatalf("outcome = %s (%s), want rolled_back from the server log's cancels",
			outcome, reason)
	}
	if after := roleSettings(t, f.pool); after != f.before {
		t.Fatalf("pg_db_role_setting after the revert = %q, before = %q", after, f.before)
	}
}

func countTimeouts(entries []logwatch.LogEntry) int {
	n := 0
	for _, e := range entries {
		if e.User == budgetRole && budget.IsStatementTimeoutCancel(e) {
			n++
		}
	}
	return n
}

// forgetRevertAction removes the fixture's action and ledger, so no later
// test's RunCycle resumes its monitor; an action another row still refers
// to is settled instead.
func forgetRevertAction(p *pgxpool.Pool, id int64) {
	ctx := context.Background()
	_, _ = p.Exec(ctx, `DELETE FROM sage.budget_shadow WHERE database_name = 'revert-fleet'`)
	_, _ = p.Exec(ctx, `DELETE FROM sage.action_outcome WHERE action_log_id = $1`, id)
	if _, err := p.Exec(ctx, `DELETE FROM sage.action_log WHERE id = $1`, id); err != nil {
		_, _ = p.Exec(ctx, `UPDATE sage.action_log SET outcome = 'failed' WHERE id = $1`, id)
	}
}
