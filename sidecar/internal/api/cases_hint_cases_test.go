package api

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// cases-30: the query-hint group keeps today's results after the split.
// No concurrency or state tests: these functions share no state and have
// no states. Invalid input: no new input path (rows come from a scan).

func TestFilterSelfMonitoringHintRows_Boundaries(t *testing.T) {
	if got := filterSelfMonitoringHintRows(nil); len(got) != 0 {
		t.Fatalf("zero rows = %v, want none", got)
	}
	if got := filterSelfMonitoringHintRows([]map[string]any{}); len(got) != 0 {
		t.Fatalf("empty rows = %v, want none", got)
	}
	self := map[string]any{"queryid": int64(1),
		"query_text": "/* pg_sage */ SELECT * FROM pg_stat_activity"}
	if got := filterSelfMonitoringHintRows([]map[string]any{self}); len(got) != 0 {
		t.Fatalf("one self row = %v, want it dropped", got)
	}
	customer := map[string]any{"queryid": int64(2),
		"query_text": "SELECT * FROM public.orders WHERE id = $1"}
	got := filterSelfMonitoringHintRows([]map[string]any{customer})
	if len(got) != 1 || got[0]["queryid"] != int64(2) {
		t.Fatalf("one customer row = %v, want it kept", got)
	}
	mixed := filterSelfMonitoringHintRows([]map[string]any{self, customer})
	if len(mixed) != 1 || mixed[0]["queryid"] != int64(2) {
		t.Fatalf("self + customer = %v, want only the customer row", mixed)
	}
}

func TestUniqueQueryHintIDs_SkipsZeroAndDuplicates(t *testing.T) {
	if got := uniqueQueryHintIDs(nil); got != nil {
		t.Fatalf("uniqueQueryHintIDs(nil) = %v, want nil", got)
	}
	rows := []map[string]any{
		{"queryid": int64(7)}, {"queryid": int64(0)}, {},
		{"queryid": "7"}, {"queryid": int32(9)}, {"queryid": "not a number"},
	}
	got := uniqueQueryHintIDs(rows)
	if len(got) != 2 || got[0] != 7 || got[1] != 9 {
		t.Fatalf("uniqueQueryHintIDs = %v, want [7 9]", got)
	}
}

func TestProjectHintRows_DropsRetiredAndSelfRows(t *testing.T) {
	// No query ids, so annotateQueryHintTexts returns before using the pool.
	selected := namedPool{name: "prod", pool: lazyPool(t)}
	rows := []map[string]any{
		{"status": "active", "hint_text": "IndexScan(a)"},
		{"status": "retired", "hint_text": "IndexScan(b)"},
		{"status": "active", "hint_text": "IndexScan(c)",
			"query_text": "/* pg_sage */ SELECT 1"},
	}
	got := projectHintRows(context.Background(), selected, rows)
	if len(got) != 1 {
		t.Fatalf("projected = %d cases, want 1 (retired and self rows dropped)", len(got))
	}
	if got[0].DatabaseName != "prod" {
		t.Fatalf("case database = %q, want prod", got[0].DatabaseName)
	}
	if rows[0]["database_name"] != "prod" {
		t.Fatalf("row database_name = %v, want prod written onto the row", rows[0])
	}
}

// A query error in queryHintCases surfaces as the database's
// unavailable marker, never as a database with no hints.
func TestQueryHintCases_ErrorMarksDatabaseUnavailable(t *testing.T) {
	closed, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	closed.Close()
	selected := namedPool{name: "prod", pool: closed}
	got, cut, err := queryHintCases(context.Background(), selected)
	if err == nil || !strings.Contains(err.Error(), "query hints:") {
		t.Fatalf("queryHintCases error = %v, want the wrapped query hints error", err)
	}
	if got != nil || cut {
		t.Fatalf("queryHintCases on error = %v cut=%v, want nil, false", got, cut)
	}
	report := mergeDatabaseCases([]namedPool{selected},
		[]databaseCases{{}}, []error{err})
	want := casesUnavailable{Database: "prod", ErrorClass: caseErrorConnection}
	if len(report.Unavailable) != 1 || report.Unavailable[0] != want {
		t.Fatalf("unavailable = %+v, want [%+v]", report.Unavailable, want)
	}
	if len(report.Cases) != 0 || report.firstErr() != err {
		t.Fatalf("report cases = %d firstErr = %v, want none and the hint error",
			len(report.Cases), report.firstErr())
	}
}
