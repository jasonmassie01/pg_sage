package analyzer

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
)

// market-11 owner checks, without a database. The parser is not needed:
// tests build the prepared form a cgo build's PrepareCheck returns.

func preparedCheck(id, sql string, funcs ...string) config.PreparedCheck {
	return config.PreparedCheck{
		CustomCheck: config.CustomCheck{
			ID: id, Title: "Check " + id, Severity: "warning", SQL: sql,
			Databases: []string{"*"}, IntervalMinutes: 5, MaxRows: 100,
			TimeoutMs: 2000, ObjectColumn: "obj", Message: "Fix " + id + ".",
		},
		Canonical: sql, Fingerprint: "fp-" + id, Functions: funcs,
	}
}

func TestCheckVerdict_Volatility(t *testing.T) {
	p := preparedCheck("v", "SELECT 1 AS obj", "random", "now")
	cases := []struct {
		procs []procInfo
		want  string
	}{
		{[]procInfo{{"random", "v"}}, "volatile"},
		{[]procInfo{{"now", "s"}, {"clock_timestamp", "v"}}, ""},
		{[]procInfo{{"upper", "i"}, {"now", "v"}}, ""},
		{[]procInfo{{"pg_sleep_for", "s"}}, "not allowed"},
		{[]procInfo{{"dblink_exec", "i"}}, "not allowed"},
		{[]procInfo{{"lo_get", "i"}}, "not allowed"},
		{[]procInfo{{"pg_read_binary_file", "i"}}, "not allowed"},
		{[]procInfo{{"pg_ls_dir", "i"}}, "not allowed"},
		{nil, ""},
	}
	for _, tc := range cases {
		err := checkVerdict(p, tc.procs)
		if tc.want == "" && err != nil {
			t.Errorf("checkVerdict(%v) = %v, want allowed", tc.procs, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("checkVerdict(%v) = %v, want %q", tc.procs, err, tc.want)
		}
	}
}

func rowsOf(n int) [][]any {
	out := make([][]any, n)
	for i := range out {
		out[i] = []any{"public.t" + strconv.Itoa(i), int64(i), nil}
	}
	return out
}

var errBoom = errors.New("boom: pg_proc unreadable")

func TestCheckFindings_ShapeAndNoSQL(t *testing.T) {
	p := preparedCheck("no_pk", "SELECT ...")
	got, truncated, err := checkFindings(p, []string{"obj", "n", "note"}, rowsOf(3))
	if err != nil || truncated || len(got) != 3 {
		t.Fatalf("checkFindings = %d findings, truncated %v, %v", len(got), truncated, err)
	}
	f := got[1]
	if f.Category != "custom:no_pk" || f.ObjectIdentifier != "public.t1" ||
		f.Severity != "warning" || f.Recommendation != "Fix no_pk." {
		t.Fatalf("finding = %+v", f)
	}
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Fatalf("a custom check carried SQL: %q / %q", f.RecommendedSQL, f.RollbackSQL)
	}
	if f.Detail["fingerprint"] != "fp-no_pk" || f.Detail["check_id"] != "no_pk" ||
		f.Detail["truncated"] != false {
		t.Fatalf("detail = %v", f.Detail)
	}
	row, _ := f.Detail["row"].(map[string]any)
	if row["n"] != "1" || row["note"] != "" {
		t.Fatalf("row detail = %v", row)
	}
}

func TestCheckFindings_TruncatesAtMaxRows(t *testing.T) {
	p := preparedCheck("many", "SELECT ...")
	got, truncated, err := checkFindings(p, []string{"obj", "n", "note"}, rowsOf(101))
	if err != nil || !truncated || len(got) != 100 {
		t.Fatalf("101 rows: %d findings, truncated %v, %v; want 100, true", len(got),
			truncated, err)
	}
	if got[0].Detail["truncated"] != true {
		t.Fatalf("detail truncated = %v", got[0].Detail["truncated"])
	}
	got, truncated, _ = checkFindings(p, []string{"obj", "n", "note"}, rowsOf(100))
	if truncated || len(got) != 100 {
		t.Fatalf("100 rows: %d, truncated %v; want 100, false", len(got), truncated)
	}
}

func TestCheckFindings_MissingObjectColumnAndEmpty(t *testing.T) {
	p := preparedCheck("bad", "SELECT ...")
	if _, _, err := checkFindings(p, []string{"other"}, rowsOf(1)); err == nil ||
		!strings.Contains(err.Error(), "obj") {
		t.Fatalf("missing object column error = %v", err)
	}
	got, truncated, err := checkFindings(p, []string{"obj"}, nil)
	if err != nil || truncated || len(got) != 0 {
		t.Fatalf("no rows = %v, %v, %v", got, truncated, err)
	}
	dup := [][]any{{"public.a"}, {"public.a"}, {nil}}
	got, _, err = checkFindings(p, []string{"obj"}, dup)
	if err != nil || len(got) != 1 {
		t.Fatalf("duplicate and null identities = %d findings, %v; want 1", len(got), err)
	}
}

func TestCustomChecks_ProjectIntoCasesWithoutActions(t *testing.T) {
	p := preparedCheck("no_pk", "SELECT ...")
	got, _, _ := checkFindings(p, []string{"obj", "n", "note"}, rowsOf(1))
	c := cases.ProjectFinding(cases.SourceFinding{
		ID: "1", Category: got[0].Category, Severity: cases.Severity(got[0].Severity),
		ObjectIdentifier: got[0].ObjectIdentifier, Title: got[0].Title,
		Recommendation: got[0].Recommendation, Detail: got[0].Detail,
	}, nil)
	if c.Title != got[0].Title || len(c.ActionCandidates) != 0 {
		t.Fatalf("case = %q with %d candidates, want the title and none", c.Title,
			len(c.ActionCandidates))
	}
}

func TestCustomChecks_DatabaseGlobAndSchedule(t *testing.T) {
	p := preparedCheck("sched", "SELECT 1 AS obj")
	p.Databases = []string{"report_*"}
	r := newCustomCheckRunner([]config.PreparedCheck{p}, nil, noopLog)
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	if r.due(p, "orders", now) {
		t.Fatal("a check for report_* is due on orders")
	}
	if !r.due(p, "report_eu", now) {
		t.Fatal("first run is not due")
	}
	r.state(p.ID).lastRun = now
	if r.due(p, "report_eu", now.Add(4*time.Minute)) {
		t.Fatal("due again inside the 5 minute interval")
	}
	if !r.due(p, "report_eu", now.Add(5*time.Minute)) {
		t.Fatal("not due at the interval")
	}
}

func TestCustomChecks_ProcLookupErrorBlocksAndLogsOnce(t *testing.T) {
	var logs []string
	logFn := func(level, msg string, args ...any) {
		if strings.Contains(msg, "custom check") {
			logs = append(logs, level)
		}
	}
	p := preparedCheck("blocked", "SELECT 1 AS obj", "upper")
	a := New(nil, phase2Config(), nil, nil, nil, nil, nil, logFn)
	a.WithCustomChecks([]config.PreparedCheck{p})
	a.checks.procLookup = func(context.Context, []string) ([]procInfo, error) {
		return nil, errBoom
	}
	clock := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	a.checks.now = func() time.Time { return clock }
	for i := 0; i < 3; i++ {
		a.eval = newCycleEval()
		if got := a.runCustomChecks(context.Background()); len(got) != 0 {
			t.Fatalf("a blocked check emitted %d findings", len(got))
		}
		if !a.eval.hasFailed("custom:blocked") {
			t.Fatalf("cycle %d: custom:blocked not failed; open findings would resolve", i)
		}
		clock = clock.Add(10 * time.Minute)
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d times, want once: %v", len(logs), logs)
	}
}
