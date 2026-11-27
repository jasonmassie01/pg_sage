package schema

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// config-04: the step runner, against a fake session. The real-server
// behaviour (locks, lock_timeout, catalog) is in bootstrap_upgrade_db_test.go.
// No concurrent-access tests here: one bootstrap session runs the steps in
// order under the advisory lock (see the parallel Bootstrap test there).

type fakeRow struct {
	present []bool
	err     error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*[]bool) = r.present
	return nil
}

// fakeStepDB is a bootstrap session: each catalog precheck takes the next
// of answers (the last repeats), each Exec the next of execErr.
type fakeStepDB struct {
	answers  [][]bool
	rowErr   error
	execErr  []error // per Exec call; nil once exhausted
	execs    []string
	queries  int
	begins   int
	commits  int
	rollback int
}

func (f *fakeStepDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	f.execs = append(f.execs, sql)
	if n := len(f.execs); n <= len(f.execErr) {
		return pgconn.CommandTag{}, f.execErr[n-1]
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeStepDB) QueryRow(context.Context, string, ...any) pgx.Row {
	f.queries++
	var present []bool
	if len(f.answers) > 0 {
		present = f.answers[min(f.queries, len(f.answers))-1]
	}
	return fakeRow{present: present, err: f.rowErr}
}

func (f *fakeStepDB) Begin(context.Context) (pgx.Tx, error) {
	f.begins++
	return fakeTx{db: f}, nil
}

// fakeTx runs on its fakeStepDB; the methods the runner does not use
// panic through the nil embedded pgx.Tx.
type fakeTx struct {
	pgx.Tx
	db *fakeStepDB
}

func (t fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}

func (t fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}

func (t fakeTx) Commit(context.Context) error {
	t.db.commits++
	return nil
}

func (t fakeTx) Rollback(context.Context) error {
	t.db.rollback++
	return nil
}

func lockErr() error {
	return fmt.Errorf("exec: %w", &pgconn.PgError{Code: "55P03",
		Message: "canceling statement due to lock timeout"})
}

func fastLockBackoff(t *testing.T) {
	t.Helper()
	saved := migrationLockBackoff
	migrationLockBackoff = []time.Duration{time.Millisecond, time.Millisecond,
		time.Millisecond, time.Millisecond}
	t.Cleanup(func() { migrationLockBackoff = saved })
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(saved) })
	return &buf
}

const testColumnStep = "ALTER TABLE sage.t ADD COLUMN IF NOT EXISTS a int"

// Nil/empty: an empty list touches no session and still logs the summary.
func TestRunMigrationSteps_EmptyListLogsTheSummary(t *testing.T) {
	logs := captureLog(t)
	db := &fakeStepDB{}
	report, err := runMigrationSteps(context.Background(), db, nil)
	if err != nil || report.steps != 0 || report.applied != 0 || report.skipped != 0 {
		t.Fatalf("empty list: report %+v err %v", report, err)
	}
	if len(db.execs) != 0 || db.queries != 0 {
		t.Fatalf("empty list ran %d statements and %d queries", len(db.execs), db.queries)
	}
	if !strings.Contains(logs.String(), "schema: 0 steps, 0 applied, 0 rerun, 0 skipped") {
		t.Fatalf("summary line missing: %q", logs.String())
	}
}

func TestRunMigrationSteps_CountsAppliedAndSkippedSteps(t *testing.T) {
	logs := captureLog(t)
	db := &fakeStepDB{answers: [][]bool{{true}, {false}}} // column present, table not
	steps := []migrationStep{
		{name: "stepSkipped", sql: testColumnStep},
		{name: "stepRerun", sql: "DO $$ BEGIN NULL; END $$"},
		{name: "stepApplied", sql: "CREATE TABLE IF NOT EXISTS sage.u (id int)"},
	}
	report, err := runMigrationSteps(context.Background(), db, steps)
	if err != nil {
		t.Fatalf("runMigrationSteps: %v", err)
	}
	if report.steps != 3 || report.applied != 1 || report.rerun != 1 ||
		report.skipped != 1 || report.slowest == "" {
		t.Fatalf("report = %+v, want 3 steps, 1 applied, 1 rerun, 1 skipped", report)
	}
	if len(db.execs) != 2 || !strings.HasPrefix(db.execs[0], "DO $$") ||
		!strings.HasPrefix(db.execs[1], "CREATE TABLE") {
		t.Fatalf("ran %q, want the DO block, then the table", db.execs)
	}
	if db.begins != 2 || db.commits != 2 {
		t.Fatalf("%d transactions, %d commits: want one per step that ran",
			db.begins, db.commits)
	}
	out := logs.String()
	for _, want := range []string{"schema: 3 steps, 1 applied, 1 rerun, 1 skipped",
		"migration step applied", "step=stepApplied"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q: %q", want, out)
		}
	}
}

func TestRunMigrationStep_PrecheckErrorNamesTheStep(t *testing.T) {
	db := &fakeStepDB{rowErr: errors.New("connection reset by peer")}
	_, err := runMigrationStep(context.Background(), db,
		migrationStep{name: "ddlBroken", sql: testColumnStep})
	if err == nil || !strings.Contains(err.Error(), "ddlBroken") ||
		!strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v, want the step and the cause", err)
	}
	if len(db.execs) != 0 {
		t.Fatalf("a failed precheck still ran %q", db.execs)
	}
}

func TestRunMigrationStep_ShortPrecheckAnswerIsAnError(t *testing.T) {
	db := &fakeStepDB{answers: [][]bool{{}}}
	_, err := runMigrationStep(context.Background(), db,
		migrationStep{name: "ddlShort", sql: testColumnStep})
	if err == nil || !strings.Contains(err.Error(), "answered 0 of 1") {
		t.Fatalf("err = %v, want a short-answer error", err)
	}
}

func TestRunMigrationStep_OtherErrorsAreNotRetried(t *testing.T) {
	fastLockBackoff(t)
	cause := &pgconn.PgError{Code: "42501", Message: "permission denied for schema sage"}
	db := &fakeStepDB{answers: [][]bool{{false}}, execErr: []error{cause}}
	_, err := runMigrationStep(context.Background(), db,
		migrationStep{name: "ddlDenied", sql: testColumnStep})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" ||
		!strings.Contains(err.Error(), "ddlDenied") {
		t.Fatalf("err = %v, want the wrapped 42501 naming the step", err)
	}
	if len(db.execs) != 1 {
		t.Fatalf("ran %d times, want 1", len(db.execs))
	}
}

// State transition: lock_timeout, then a granted lock, applies the step.
func TestRunMigrationStep_RetriesALockTimeoutThenApplies(t *testing.T) {
	fastLockBackoff(t)
	db := &fakeStepDB{answers: [][]bool{{false}}, execErr: []error{lockErr(), lockErr()}}
	outcome, err := runMigrationStep(context.Background(), db,
		migrationStep{name: "ddlBusy", sql: testColumnStep})
	if err != nil || outcome != stepApplied || len(db.execs) != 3 {
		t.Fatalf("outcome %d err %v after %d attempts, want applied on the 3rd",
			outcome, err, len(db.execs))
	}
	if db.begins != 3 || db.commits != 1 || db.rollback != 3 {
		t.Fatalf("begins %d commits %d rollbacks %d: want each attempt in its own "+
			"transaction, only the last committed", db.begins, db.commits, db.rollback)
	}
}

// Boundary: the last backoff used, the step fails naming lock_timeout.
func TestRunMigrationStep_GivesUpAfterTheLastBackoff(t *testing.T) {
	fastLockBackoff(t)
	errs := make([]error, 10)
	for i := range errs {
		errs[i] = lockErr()
	}
	db := &fakeStepDB{answers: [][]bool{{false}}, execErr: errs}
	_, err := runMigrationStep(context.Background(), db,
		migrationStep{name: "ddlBusy", sql: testColumnStep})
	if want := len(migrationLockBackoff) + 1; len(db.execs) != want {
		t.Fatalf("%d attempts, want %d", len(db.execs), want)
	}
	if err == nil || !isLockNotAvailable(err) {
		t.Fatalf("err = %v, want the wrapped 55P03", err)
	}
	for _, want := range []string{"ddlBusy", "lock_timeout 2s", "5 attempts"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestWaitLockRetry_Boundaries(t *testing.T) {
	fastLockBackoff(t)
	if !waitLockRetry(context.Background(), 0) {
		t.Error("attempt 0 with time left: no retry")
	}
	if waitLockRetry(context.Background(), len(migrationLockBackoff)) {
		t.Error("retried past the last backoff")
	}
	short, cancel := context.WithTimeout(context.Background(), migrationLockTimeout/2)
	defer cancel()
	if waitLockRetry(short, 0) {
		t.Error("retried with less budget left than one lock_timeout")
	}
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if waitLockRetry(done, 0) {
		t.Error("retried on a cancelled context")
	}
}

func TestMigrationBudget_SumsStepsUpToTheCap(t *testing.T) {
	cases := map[int]time.Duration{
		1: 10 * time.Second, 29: 290 * time.Second, 30: migrationBootCap,
		31: migrationBootCap, 76: migrationBootCap,
	}
	for steps, want := range cases {
		if got := migrationBudget(steps); got != want {
			t.Errorf("migrationBudget(%d) = %s, want %s", steps, got, want)
		}
	}
}

func TestIsLockNotAvailable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{lockErr(), true},
		{&pgconn.PgError{Code: "57014"}, false},
		{errors.New("55P03"), false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := isLockNotAvailable(tc.err); got != tc.want {
			t.Errorf("isLockNotAvailable(%v) = %t, want %t", tc.err, got, tc.want)
		}
	}
}

func TestMigrationReport_String(t *testing.T) {
	r := migrationReport{steps: 76, applied: 3, skipped: 73,
		elapsed: 412 * time.Millisecond, slowest: "ddlPerfIndexes",
		slowestTook: 180 * time.Millisecond}
	r.rerun, r.skipped = 40, 33
	want := "schema: 76 steps, 3 applied, 40 rerun, 33 skipped, 412 ms; " +
		"slowest ddlPerfIndexes 180 ms"
	if got := r.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got := (migrationReport{steps: 2, skipped: 2}).String(); strings.Contains(got,
		"slowest") {
		t.Fatalf("no step applied, yet %q names a slowest", got)
	}
}

// State transition: an opaque statement that drops an index a later
// statement of the step re-creates makes the runner read the facts again.
func TestApplyStep_RereadsFactsAfterAnOpaqueStatement(t *testing.T) {
	stmts := classifyStep(`ALTER TABLE sage.t ADD COLUMN IF NOT EXISTS a int;
DO $$ BEGIN DROP INDEX IF EXISTS sage.i; END $$;
CREATE UNIQUE INDEX IF NOT EXISTS i ON sage.t (a);
UPDATE sage.t SET a = 0 WHERE a IS NULL`)
	// Before the DO: column and index present. After it: the index is gone.
	db := &fakeStepDB{answers: [][]bool{{true, true}, {false}}}
	outcome, err := applyStep(context.Background(), db, stmts)
	if err != nil || outcome != stepApplied {
		t.Fatalf("applyStep: outcome %d err %v, want applied", outcome, err)
	}
	if db.queries != 2 || len(db.execs) != 2 {
		t.Fatalf("%d fact reads, execs %q: want 2 reads and 2 batches", db.queries, db.execs)
	}
	if !strings.HasPrefix(db.execs[0], "\nDO $$") ||
		!strings.Contains(db.execs[1], "CREATE UNIQUE INDEX") ||
		!strings.Contains(db.execs[1], "UPDATE sage.t") ||
		strings.Contains(strings.Join(db.execs, ";"), "ADD COLUMN") {
		t.Fatalf("execs %q: want the DO, then the index and the backfill", db.execs)
	}
}

// A step of every-boot statements only reruns; its facts are never read
// again when nothing after them has one.
func TestApplyStep_EveryBootOnlyIsARerunInOneBatch(t *testing.T) {
	db := &fakeStepDB{}
	outcome, err := applyStep(context.Background(), db, classifyStep(
		"SET LOCAL lock_timeout = '5s'; DO $$ BEGIN NULL; END $$; UPDATE sage.t SET a = 1"))
	if err != nil || outcome != stepRerun || len(db.execs) != 1 || db.queries != 0 {
		t.Fatalf("outcome %d err %v execs %q queries %d: want one rerun batch",
			outcome, err, db.execs, db.queries)
	}
	if !strings.HasPrefix(db.execs[0], "SET LOCAL") {
		t.Fatalf("batch %q lost its leading setting", db.execs[0])
	}
}
