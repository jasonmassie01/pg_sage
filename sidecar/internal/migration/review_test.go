package migration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// statsQuerier answers Review's catalog reads from fixed values: the
// table has rows (analyzed), the column has colType, nothing else is
// active. failOn makes every read whose SQL contains it fail with err.
type statsQuerier struct {
	rows    int64
	colType string
	failOn  string
	err     error
	mu      sync.Mutex
	reads   int
}

func (q *statsQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("statsQuerier: Query not supported (use qualified names)")
}

func (q *statsQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.mu.Lock()
	q.reads++
	q.mu.Unlock()
	if q.failOn != "" && strings.Contains(sql, q.failOn) {
		return errRow{q.err}
	}
	return scanRow(func(dest []any) error { return q.fill(sql, dest) })
}

func (q *statsQuerier) fill(sql string, dest []any) error {
	switch {
	case strings.Contains(sql, "server_version_num"):
		*(dest[0].(*int)) = 170000
	case strings.Contains(sql, "reltuples"):
		*(dest[0].(*int64)), *(dest[1].(*int64)) = q.rows, q.rows*100
	case strings.Contains(sql, "pg_locks"): // before pg_stat_activity: it joins it
		*(dest[0].(*int)) = 0
	case strings.Contains(sql, "pg_stat_activity") && strings.Contains(sql, "COUNT"):
		*(dest[0].(*int)), *(dest[1].(*float64)) = 0, 0
	case strings.Contains(sql, "pg_stat_replication"):
		*(dest[0].(*float64)) = 0
	case strings.Contains(sql, "pg_available_extensions"):
		*(dest[0].(*bool)) = false
	case strings.Contains(sql, "format_type"):
		if q.colType == "" {
			return pgx.ErrNoRows
		}
		*(dest[0].(*string)) = q.colType
	default:
		return errors.New("statsQuerier: unexpected SQL " + sql)
	}
	return nil
}

type scanRow func(dest []any) error

func (r scanRow) Scan(dest ...any) error { return r(dest) }

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

func mustReview(t *testing.T, q *statsQuerier, sql string, o ReviewOptions) Report {
	t.Helper()
	var querier catalogread.Querier
	if q != nil {
		querier = q
	}
	rep, err := Review(context.Background(), querier, sql, o)
	if err != nil {
		t.Fatalf("Review(%q): %v", sql, err)
	}
	return rep
}

func onlyHazard(t *testing.T, rep Report, rule string) Hazard {
	t.Helper()
	for _, st := range rep.Statements {
		for _, h := range st.Hazards {
			if h.RuleID == rule {
				return h
			}
		}
	}
	t.Fatalf("no %s hazard in %+v", rule, rep.Statements)
	return Hazard{}
}

// Happy path: a plain CREATE INDEX on a 1M-row table blocks writes for the
// whole build: risky, exit 2, with the CONCURRENTLY rewrite.
func TestReviewCreateIndexOnLargeTableIsRisky(t *testing.T) {
	rep := mustReview(t, &statsQuerier{rows: 1_000_000}, "CREATE INDEX x ON app.t (c)",
		ReviewOptions{})
	if rep.Verdict != VerdictRisky || rep.ExitCode() != 2 {
		t.Fatalf("verdict %q exit %d, want risky/2: %+v", rep.Verdict, rep.ExitCode(), rep)
	}
	h := onlyHazard(t, rep, "ddl_index_not_concurrent")
	if !h.SizeKnown || h.LiveRows != 1_000_000 || h.LockLevel != "SHARE" || h.Table != "app.t" {
		t.Fatalf("hazard %+v, want SHARE on app.t with 1,000,000 rows", h)
	}
	got := rep.Statements[0].GuardedRewrite
	if !strings.HasPrefix(got, "CREATE INDEX CONCURRENTLY x ON app.t (c);") {
		t.Fatalf("guarded rewrite %q, want the CONCURRENTLY form", got)
	}
	if rep.Static || rep.PGVersion != 170000 || rep.RowThreshold != defaultDDLRowThreshold {
		t.Fatalf("report header %+v", rep)
	}
}

// Boundary: below ddl_row_threshold a metadata-only ACCESS EXCLUSIVE is
// review; at the threshold, with no lock_timeout, it is risky; with
// lock_timeout set it stays review. The configured threshold is applied.
func TestReviewRowThresholdBoundary(t *testing.T) {
	const drop = "ALTER TABLE app.t DROP COLUMN b"
	cases := []struct {
		name      string
		rows      int64
		threshold int64
		sql       string
		want      string
	}{
		{"threshold-1", 9_999, 0, drop, VerdictReview},
		{"threshold", 10_000, 0, drop, VerdictRisky},
		{"threshold with lock_timeout", 10_000, 0, "SET lock_timeout = '5s'; " + drop,
			VerdictReview},
		{"configured threshold-1", 499, 500, drop, VerdictReview},
		{"configured threshold", 500, 500, drop, VerdictRisky},
	}
	for _, tc := range cases {
		rep := mustReview(t, &statsQuerier{rows: tc.rows}, tc.sql,
			ReviewOptions{RowThreshold: tc.threshold})
		if rep.Verdict != tc.want {
			t.Errorf("%s: verdict %q (score %.3f), want %q", tc.name, rep.Verdict,
				rep.MaxRiskScore, tc.want)
		}
	}
}

// Invalid input: empty, whitespace-only and over-limit text and an
// unsupported version are invalid; 100,000 bytes is the inclusive limit.
func TestReviewInvalidInput(t *testing.T) {
	for _, sql := range []string{"", "  \n\t"} {
		if _, err := Review(context.Background(), nil, sql, ReviewOptions{}); !errors.Is(err,
			ErrInvalidMigration) {
			t.Errorf("Review(%q) err %v, want ErrInvalidMigration", sql, err)
		}
	}
	at := "SELECT 1" + strings.Repeat(" ", MaxReviewBytes-len("SELECT 1"))
	if _, err := Review(context.Background(), nil, at, ReviewOptions{}); err != nil {
		t.Fatalf("%d bytes: %v", len(at), err)
	}
	_, err := Review(context.Background(), nil, at+" ", ReviewOptions{})
	if !errors.Is(err, ErrInvalidMigration) || !strings.Contains(err.Error(), "100001 bytes") {
		t.Fatalf("over the limit: %v", err)
	}
	_, err = Review(context.Background(), nil, "SELECT 1", ReviewOptions{PGVersion: 13})
	if !errors.Is(err, ErrInvalidMigration) {
		t.Fatalf("pg_version 13: %v", err)
	}
}

// An unparseable or unrecognized statement is review (exit 1) with a
// message, never safe.
func TestReviewUnrecognizedIsReview(t *testing.T) {
	for _, sql := range []string{"CREATE FROBNICATE x", "DO $$ BEGIN PERFORM 1; END $$"} {
		rep := mustReview(t, nil, sql, ReviewOptions{})
		st := rep.Statements[0]
		if rep.Verdict != VerdictReview || rep.ExitCode() != 1 ||
			st.Status != StatementUnrecognized || !strings.Contains(st.Note, "not proven") {
			t.Fatalf("%q: %+v", sql, rep)
		}
		if !hasNote(rep, "unrecognized DDL is not proven safe") {
			t.Fatalf("%q: notes %v", sql, rep.Notes)
		}
	}
}

func hasNote(rep Report, sub string) bool {
	for _, n := range rep.Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// Nil/empty: known SQL with no hazard is safe (exit 0) and carries no
// "not proven" note; a migration of only comments has no statement.
func TestReviewNoDDLIsSafe(t *testing.T) {
	rep := mustReview(t, nil, "CREATE TABLE app.n (id int); CREATE INDEX CONCURRENTLY "+
		"n_id ON app.n (id)", ReviewOptions{})
	if rep.Verdict != VerdictSafe || rep.ExitCode() != 0 || len(rep.Statements) != 2 ||
		rep.MaxRiskScore != 0 || hasNote(rep, "not proven") {
		t.Fatalf("report %+v, want safe with no not-proven note", rep)
	}
	for _, st := range rep.Statements {
		if len(st.Hazards) != 0 || st.GuardedRewrite != "" || st.LockTimeoutMissing {
			t.Fatalf("statement %+v, want no hazard and no rewrite", st)
		}
	}
	rep = mustReview(t, nil, "-- nothing to do\n/* still nothing */", ReviewOptions{})
	if rep.Verdict != VerdictSafe || len(rep.Statements) != 0 {
		t.Fatalf("comments only: %+v", rep)
	}
}

// Error propagation: a catalog read that fails (a timeout) is a tool
// error, never a verdict; a version read that fails stops the review.
func TestReviewCatalogFailureIsAnError(t *testing.T) {
	timeout := errors.New("canceling statement due to statement timeout")
	q := &statsQuerier{rows: 50, failOn: "reltuples", err: timeout}
	rep, err := Review(context.Background(), q, "CREATE INDEX x ON app.t (c)", ReviewOptions{})
	if !errors.Is(err, ErrCatalogUnavailable) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err %v, want ErrCatalogUnavailable naming the timeout", err)
	}
	if rep.Verdict == VerdictSafe {
		t.Fatalf("a migration that could not be measured read safe: %+v", rep)
	}
	q = &statsQuerier{failOn: "server_version_num", err: errors.New("connection refused")}
	_, err = Review(context.Background(), q, "CREATE INDEX x ON app.t (c)", ReviewOptions{})
	if !errors.Is(err, ErrCatalogUnavailable) || !strings.Contains(err.Error(), "connection") {
		t.Fatalf("version read failure: %v", err)
	}
	q = &statsQuerier{failOn: "format_type", err: timeout}
	_, err = Review(context.Background(), q, "ALTER TABLE app.t ALTER COLUMN v TYPE text",
		ReviewOptions{})
	if !errors.Is(err, ErrCatalogUnavailable) {
		t.Fatalf("column type read failure: %v", err)
	}
}

// A missing table (no row) is an answer, not a failure.
func TestReviewMissingTableIsNotAnError(t *testing.T) {
	q := &statsQuerier{failOn: "reltuples", err: pgx.ErrNoRows}
	rep := mustReview(t, q, "CREATE INDEX x ON app.new_table (c)", ReviewOptions{})
	h := onlyHazard(t, rep, "ddl_index_not_concurrent")
	if h.SizeKnown || h.Size != "unknown" || rep.Verdict != VerdictReview {
		t.Fatalf("hazard %+v verdict %q, want size unknown and review", h, rep.Verdict)
	}
}

// Static mode reads nothing: sizes are unknown, an ACCESS EXCLUSIVE or a
// rewrite is never below review, and an intrinsic 1.0 is risky.
func TestReviewStaticMode(t *testing.T) {
	q := &statsQuerier{rows: 1_000_000}
	rep := mustReview(t, q, "CREATE INDEX x ON app.t (c)", ReviewOptions{Static: true,
		PGVersion: 16})
	if q.reads != 0 || !rep.Static || rep.PGVersion != 160000 {
		t.Fatalf("static review read %d times: %+v", q.reads, rep)
	}
	if h := onlyHazard(t, rep, "ddl_index_not_concurrent"); h.SizeKnown || h.Size != "unknown" {
		t.Fatalf("static hazard %+v, want size unknown", h)
	}
	if rep.Verdict != VerdictReview || !hasNote(rep, "Static review") {
		t.Fatalf("static CREATE INDEX: %+v", rep)
	}
	rep = mustReview(t, nil, "ALTER TABLE app.t DROP COLUMN b", ReviewOptions{})
	if rep.Verdict != VerdictReview || rep.PGVersion != oldestSupportedVersion {
		t.Fatalf("static DROP COLUMN: %+v", rep)
	}
	rep = mustReview(t, nil, "ALTER TABLE app.t ALTER COLUMN v TYPE bigint", ReviewOptions{})
	if rep.Verdict != VerdictRisky {
		t.Fatalf("static rewrite under ACCESS EXCLUSIVE: %+v", rep)
	}
}

// lock_timeout is the advisor's annotation, not a scored hazard: it marks
// the statement and the guarded rewrite sets it.
func TestReviewLockTimeoutIsAnnotation(t *testing.T) {
	rep := mustReview(t, nil, "ALTER TABLE app.t ALTER COLUMN a SET NOT NULL", ReviewOptions{})
	st := rep.Statements[0]
	for _, h := range st.Hazards {
		if h.RuleID == lockTimeoutRuleID {
			t.Fatalf("lock_timeout scored as a hazard: %+v", st.Hazards)
		}
	}
	if !st.LockTimeoutMissing || !strings.HasPrefix(st.GuardedRewrite, guardLockTimeout) ||
		!strings.Contains(st.GuardedRewrite, "VALIDATE CONSTRAINT a_not_null") {
		t.Fatalf("statement %+v, want the annotation and the CHECK rewrite", st)
	}
	rep = mustReview(t, nil, "SET lock_timeout = '5s'; ALTER TABLE app.t ALTER COLUMN a "+
		"SET NOT NULL", ReviewOptions{})
	if rep.Statements[1].LockTimeoutMissing ||
		strings.Contains(rep.Statements[1].GuardedRewrite, "lock_timeout") {
		t.Fatalf("lock_timeout already set: %+v", rep.Statements[1])
	}
}

// Lint and the advisor agree on varchar -> text: no rewrite.
func TestReviewBinaryCoercibleTypeChange(t *testing.T) {
	sql := "ALTER TABLE app.t ALTER COLUMN v TYPE text"
	rep := mustReview(t, &statsQuerier{rows: 10, colType: "character varying(20)"}, sql,
		ReviewOptions{})
	if h := onlyHazard(t, rep, "ddl_alter_type_rewrite"); h.RequiresRewrite {
		t.Fatalf("varchar(20) -> text still a rewrite: %+v", h)
	}
	rep = mustReview(t, &statsQuerier{rows: 10, colType: "integer"}, sql, ReviewOptions{})
	if h := onlyHazard(t, rep, "ddl_alter_type_rewrite"); !h.RequiresRewrite {
		t.Fatalf("integer -> text lost its rewrite: %+v", h)
	}
}

// Concurrent access: parallel reviews share one querier and agree.
// (-race is unavailable on this machine: no cgo.)
func TestReviewConcurrent(t *testing.T) {
	q := &statsQuerier{rows: 1_000_000}
	var wg sync.WaitGroup
	verdicts := make([]string, 50)
	for i := range verdicts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rep, err := Review(context.Background(), q, "CREATE INDEX x ON app.t (c)",
				ReviewOptions{})
			if err != nil {
				verdicts[i] = err.Error()
				return
			}
			verdicts[i] = rep.Verdict
		}(i)
	}
	wg.Wait()
	for i, v := range verdicts {
		if v != VerdictRisky {
			t.Fatalf("review %d: %q, want risky", i, v)
		}
	}
}

func TestReportExitCodes(t *testing.T) {
	for verdict, want := range map[string]int{VerdictSafe: 0, VerdictReview: 1,
		VerdictRisky: 2, "": 3, "bogus": 3} {
		if got := (Report{Verdict: verdict}).ExitCode(); got != want {
			t.Errorf("ExitCode(%q) = %d, want %d", verdict, got, want)
		}
	}
}

// Statement lines skip a ';' in a literal, a comment and a quoted name.
func TestStatementLines(t *testing.T) {
	sql := "-- header; not a statement\nCREATE TABLE a (x text DEFAULT ';');\n\n" +
		"/* block ; */\nCREATE INDEX \"i;x\" ON a (x);\n  ALTER TABLE a\n  DROP COLUMN x"
	got := statementLines(sql, 3)
	want := []int{2, 5, 6}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lines %v, want %v", got, want)
		}
	}
	if got := statementLines(sql, 2); got[0] != 0 || got[1] != 0 {
		t.Fatalf("a count mismatch must give zeros, got %v", got)
	}
	rep := mustReview(t, nil, sql, ReviewOptions{})
	if rep.Statements[2].Line != 6 {
		t.Fatalf("report lines %+v", rep.Statements)
	}
}
