package startup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// fakeRow scans one canned answer.
type fakeRow struct{ scan func(dest ...any) error }

func (r fakeRow) Scan(dest ...any) error { return r.scan(dest...) }

// fakeChecksDB answers the startup checks without a server. failColumn
// fails the column check for that column; failVisibility fails the
// visibility check.
type fakeChecksDB struct {
	mu             sync.Mutex
	schema         string
	visible        bool
	columns        map[string]bool
	failColumn     map[string]error
	failVisibility error
	columnRels     []string
	readSQL        []string
}

func (f *fakeChecksDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(sql, "pg_attribute"):
		rel, col := args[0].(string), args[1].(string)
		f.columnRels = append(f.columnRels, rel)
		if err := f.failColumn[col]; err != nil {
			return fakeRow{func(...any) error { return err }}
		}
		return fakeRow{scanBool(f.columns[col])}
	case strings.Contains(sql, "pg_has_role"):
		if f.failVisibility != nil {
			return fakeRow{func(...any) error { return f.failVisibility }}
		}
		return fakeRow{scanBool(f.visible)}
	case strings.Contains(sql, "pg_extension"):
		schema := f.schema
		return fakeRow{func(dest ...any) error { *dest[0].(*string) = schema; return nil }}
	case strings.Contains(sql, "count(*)"):
		f.readSQL = append(f.readSQL, sql)
		return fakeRow{scanInt(7)}
	case strings.Contains(sql, "server_version_num"):
		return fakeRow{scanInt(170004)}
	case strings.Contains(sql, "SELECT 1"):
		return fakeRow{scanInt(1)}
	}
	return fakeRow{func(...any) error { return fmt.Errorf("unexpected SQL %q", sql) }}
}

func scanBool(v bool) func(dest ...any) error {
	return func(dest ...any) error { *dest[0].(*bool) = v; return nil }
}

func scanInt(v int) func(dest ...any) error {
	return func(dest ...any) error { *dest[0].(*int) = v; return nil }
}

func healthyFake() *fakeChecksDB {
	return &fakeChecksDB{schema: "ext", visible: true,
		columns: map[string]bool{"wal_records": true, "total_plan_time": true}}
}

func TestRunChecksReadsTheExtensionSchemaQualified(t *testing.T) {
	db := healthyFake()
	got, err := runChecks(context.Background(), db)
	if err != nil {
		t.Fatalf("runChecks: %v", err)
	}
	if got.StatementsSchema != "ext" || !got.HasWALColumns || !got.HasPlanTimeColumns ||
		!got.QueryTextVisible || got.PGVersionNum != 170004 {
		t.Fatalf("result = %+v, want schema ext, both columns, visible, 170004", *got)
	}
	if len(got.CapabilityErrors) != 0 {
		t.Fatalf("CapabilityErrors = %v, want none", got.CapabilityErrors)
	}
	want := `"ext".pg_stat_statements`
	if len(db.columnRels) != 2 || db.columnRels[0] != want || db.columnRels[1] != want {
		t.Fatalf("column checks probed %v, want %s twice", db.columnRels, want)
	}
	if len(db.readSQL) != 1 || !strings.Contains(db.readSQL[0], "FROM "+want) {
		t.Fatalf("readable check ran %v, want a read FROM %s", db.readSQL, want)
	}
}

// A column check that times out is an unknown capability: it names the
// cause, keeps the column off and does not fail the checks.
func TestRunChecksRecordsColumnCheckTimeout(t *testing.T) {
	db := healthyFake()
	timeout := fmt.Errorf("query: %w", context.DeadlineExceeded)
	db.failColumn = map[string]error{"wal_records": timeout}
	got, err := runChecks(context.Background(), db)
	if err != nil {
		t.Fatalf("runChecks with a timed-out column check: %v, want no fatal error", err)
	}
	if got.HasWALColumns {
		t.Error("HasWALColumns = true after the check timed out, want false (unknown)")
	}
	if !got.HasPlanTimeColumns {
		t.Error("HasPlanTimeColumns = false, want true: only the WAL check failed")
	}
	cause := got.CapabilityErrors[CapabilityWALColumns]
	if !errors.Is(cause, context.DeadlineExceeded) {
		t.Fatalf("CapabilityErrors[%s] = %v, want the deadline", CapabilityWALColumns, cause)
	}
	if !strings.Contains(cause.Error(), "wal_records") {
		t.Errorf("cause %q does not name the column", cause)
	}
	if _, ok := got.CapabilityErrors[CapabilityPlanTimeColumns]; ok {
		t.Errorf("plan_time_columns recorded as failed: %v", got.CapabilityErrors)
	}
}

func TestRunChecksRecordsBothColumnErrors(t *testing.T) {
	db := healthyFake()
	denied := errors.New("permission denied for view pg_stat_statements")
	db.failColumn = map[string]error{"wal_records": denied, "total_plan_time": denied}
	got, err := runChecks(context.Background(), db)
	if err != nil {
		t.Fatalf("runChecks: %v", err)
	}
	if got.HasWALColumns || got.HasPlanTimeColumns || len(got.CapabilityErrors) != 2 {
		t.Fatalf("result = %+v, want both columns off and two capability errors", *got)
	}
	warnings := got.CapabilityWarnings()
	if len(warnings) != 2 {
		t.Fatalf("CapabilityWarnings = %v, want 2", warnings)
	}
	if !strings.Contains(warnings[0], "plan_time_columns") ||
		!strings.Contains(warnings[1], "wal_columns") {
		t.Errorf("warnings %v, want sorted by key", warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w, "permission denied") || !strings.Contains(w, "not collected") {
			t.Errorf("warning %q must name the cause and the effect", w)
		}
	}
}

// A column check that fails because the caller's context ended is not a
// capability problem: the checks fail.
func TestRunChecksColumnErrorAfterContextEndIsFatal(t *testing.T) {
	db := healthyFake()
	ctx, cancel := context.WithCancel(context.Background())
	db.failColumn = map[string]error{"wal_records": context.Canceled}
	db.columns = map[string]bool{"total_plan_time": true}
	cancel()
	// The fake ignores the context, so only the column check fails.
	got, err := runChecks(ctx, db)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("runChecks after cancel = %+v, %v; want a context error", got, err)
	}
	if errors.Is(err, ErrStatementsUnavailable) {
		t.Errorf("err %v marked degradable", err)
	}
}

func TestRunChecksVisibilityErrorIsReturned(t *testing.T) {
	db := healthyFake()
	db.failVisibility = errors.New("conn closed")
	got, err := runChecks(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "conn closed") {
		t.Fatalf("runChecks = %+v, %v; want the visibility error", got, err)
	}
	if !strings.Contains(err.Error(), "query text visibility") {
		t.Errorf("err %q does not name the check", err)
	}
}

func TestCheckQueryTextVisibleUsesRoleMembership(t *testing.T) {
	for _, visible := range []bool{true, false} {
		db := healthyFake()
		db.visible = visible
		got, err := checkQueryTextVisible(context.Background(), db)
		if err != nil || got != visible {
			t.Errorf("visible=%v: got %v, %v", visible, got, err)
		}
	}
}

func TestCapabilityWarnings_NilAndEmpty(t *testing.T) {
	var nilResult *CheckResult
	if w := nilResult.CapabilityWarnings(); len(w) != 0 {
		t.Errorf("nil result warnings = %v, want none", w)
	}
	if w := (&CheckResult{}).CapabilityWarnings(); len(w) != 0 {
		t.Errorf("empty result warnings = %v, want none", w)
	}
}

func TestStatementsRelation(t *testing.T) {
	cases := map[string]string{
		"public":    `"public".pg_stat_statements`,
		"ext":       `"ext".pg_stat_statements`,
		`Odd "S"`:   `"Odd ""S""".pg_stat_statements`,
		"":          "pg_stat_statements",
		"a.b; drop": `"a.b; drop".pg_stat_statements`,
	}
	for schema, want := range cases {
		if got := statementsRelation(schema); got != want {
			t.Errorf("statementsRelation(%q) = %s, want %s", schema, got, want)
		}
	}
}

func TestMissingExtensionMessagePointsAtFirstLook(t *testing.T) {
	db := healthyFake()
	missing := errors.New("no rows in result set")
	_, err := checkExtensionInstalled(context.Background(), failInstalled{db, missing})
	if err == nil {
		t.Fatal("checkExtensionInstalled with no row: want an error")
	}
	for _, want := range []string{"pg_stat_statements extension is not installed",
		"CREATE EXTENSION pg_stat_statements", "shared_preload_libraries", "first look",
		"no rows in result set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// failInstalled fails the pg_extension lookup.
type failInstalled struct {
	*fakeChecksDB
	err error
}

func (f failInstalled) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "pg_extension") {
		return fakeRow{func(...any) error { return f.err }}
	}
	return f.fakeChecksDB.QueryRow(ctx, sql, args...)
}
