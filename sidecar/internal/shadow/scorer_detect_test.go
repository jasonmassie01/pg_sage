package shadow

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/extstats"
	"github.com/pg-sage/sidecar/internal/verify"
)

// earned-05: shadow evidence for self-initiated classes beyond index
// create and drop. A manual VACUUM or ANALYZE (never autovacuum), an
// autovacuum reloption set to exactly the proposed value, or the same
// extended statistics object, made outside pg_sage, is external evidence;
// config_guc, query_hint and reindex are never scored from the catalog.
// No concurrent access tests here: these are pure functions; the racing
// scorers are in scorer_detect_db_test.go.

func TestDetectorTableCoversTheScoredClassesOnly(t *testing.T) {
	for _, class := range []string{"index_create", "index_drop", "vacuum", "analyze",
		"autovacuum_tuning", "statistics"} {
		if _, ok := detectors[class]; !ok {
			t.Errorf("class %s has no catalog detector", class)
		}
	}
	for _, class := range []string{"config_guc", "query_hint", "reindex", "retention",
		"index_replace", ""} {
		if _, ok := detectors[class]; ok {
			t.Errorf("class %q must not be scored from the catalog", class)
		}
	}
}

func TestMaintenanceSignalReadsOnlyManualRuns(t *testing.T) {
	for _, col := range []string{"last_vacuum", "last_analyze", "stats_reset"} {
		if !strings.Contains(maintenanceSignalSQL, col) {
			t.Errorf("the maintenance signal does not read %s", col)
		}
	}
	for _, col := range []string{"last_autovacuum", "last_autoanalyze"} {
		if strings.Contains(maintenanceSignalSQL, col) {
			t.Errorf("the maintenance signal reads %s: autovacuum is not a decision "+
				"applied outside pg_sage", col)
		}
	}
	if !strings.HasPrefix(maintenanceSignalSQL, "/* pg_sage") {
		t.Error("the maintenance signal is not tagged as pg_sage's own query")
	}
}

func TestParseReloptions(t *testing.T) {
	cases := []struct {
		name, sql, table string
		want             []string
		ok               bool
	}{
		{"one option", `ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.05)`,
			"public.t", []string{"autovacuum_vacuum_scale_factor=0.05"}, true},
		{"quoted value, two options",
			`alter table "Sales".orders set (autovacuum_vacuum_threshold='50', ` +
				`AUTOVACUUM_ANALYZE_SCALE_FACTOR = 0.02);`,
			`"Sales".orders`, []string{"autovacuum_vacuum_threshold=50",
				"autovacuum_analyze_scale_factor=0.02"}, true},
		{"if exists", `ALTER TABLE IF EXISTS public.t SET (fillfactor = 70)`,
			"public.t", []string{"fillfactor=70"}, true},
		{"reset", `ALTER TABLE public.t RESET (autovacuum_vacuum_scale_factor)`, "", nil, false},
		{"no value", `ALTER TABLE public.t SET (autovacuum_enabled)`, "", nil, false},
		{"not alter table", `ALTER SYSTEM SET work_mem = '64MB'`, "", nil, false},
		{"other alter", `ALTER TABLE public.t ADD COLUMN c int`, "", nil, false},
		{"empty list", `ALTER TABLE public.t SET ()`, "", nil, false},
		{"trailing text", `ALTER TABLE public.t SET (fillfactor = 70), SET TABLESPACE x`,
			"", nil, false},
		{"empty", ``, "", nil, false},
	}
	for _, tc := range cases {
		table, opts, ok := parseReloptions(tc.sql)
		if ok != tc.ok || table != tc.table || !slices.Equal(opts, tc.want) {
			t.Errorf("%s: parseReloptions = %q %q %v, want %q %q %v", tc.name, table, opts,
				ok, tc.table, tc.want, tc.ok)
		}
	}
}

func TestReloptionsMatchExactly(t *testing.T) {
	want := []string{"autovacuum_vacuum_scale_factor=0.05"}
	for _, tc := range []struct {
		name string
		have []string
		ok   bool
	}{
		{"exact", []string{"fillfactor=70", "autovacuum_vacuum_scale_factor=0.05"}, true},
		{"other value", []string{"autovacuum_vacuum_scale_factor=0.1"}, false},
		{"same digits written longer", []string{"autovacuum_vacuum_scale_factor=0.050"}, false},
		{"prefix only", []string{"autovacuum_vacuum_scale_factor=0.05x"}, false},
		{"none", nil, false},
	} {
		if got := reloptionsMatch(tc.have, want); got != tc.ok {
			t.Errorf("%s: reloptionsMatch(%q) = %v, want %v", tc.name, tc.have, got, tc.ok)
		}
	}
	if reloptionsMatch([]string{"a=1"}, nil) {
		t.Error("an empty proposal must never match")
	}
	if !reloptionsMatch([]string{"a=1", "b=2"}, []string{"b=2", "a=1"}) {
		t.Error("every proposed option present must match in any order")
	}
	if reloptionsMatch([]string{"a=1"}, []string{"a=1", "b=2"}) {
		t.Error("one proposed option missing must not match")
	}
}

func TestStatisticsMatch(t *testing.T) {
	all, err := extstats.ParseCreate(
		`CREATE STATISTICS public.sage_stx_t ON a, b FROM public.t`)
	if err != nil {
		t.Fatal(err)
	}
	two, err := extstats.ParseCreate(
		`CREATE STATISTICS public.sage_stx_t (dependencies, ndistinct) ON b, a FROM public.t`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		c       extstats.Create
		kinds   []string
		columns []string
		exprs   bool
		ok      bool
	}{
		{"all kinds by default", all, []string{"d", "f", "m"}, []string{"a", "b"}, false, true},
		{"kinds in any order", two, []string{"f", "d"}, []string{"a", "b"}, false, true},
		{"missing a kind", all, []string{"d", "f"}, []string{"a", "b"}, false, false},
		{"extra kind", two, []string{"d", "f", "m"}, []string{"a", "b"}, false, false},
		{"other columns", all, []string{"d", "f", "m"}, []string{"a", "c"}, false, false},
		{"fewer columns", all, []string{"d", "f", "m"}, []string{"a"}, false, false},
		{"with expressions", all, []string{"d", "f", "m", "e"}, []string{"a", "b"}, true,
			false},
		{"nothing", all, nil, nil, false, false},
	} {
		if got := statisticsMatch(tc.c, tc.kinds, tc.columns, tc.exprs); got != tc.ok {
			t.Errorf("%s: statisticsMatch = %v, want %v", tc.name, got, tc.ok)
		}
	}
}

func TestUnscorableReason(t *testing.T) {
	denied := fmt.Errorf("read: %w", &pgconn.PgError{Code: "42501",
		Message: "permission denied for schema secret"})
	if reason, ok := unscorableReason(denied); !ok ||
		!strings.Contains(reason, "permission denied") || !strings.Contains(reason, "42501") {
		t.Fatalf("42501: %q %v", reason, ok)
	}
	gone := &pgconn.PgError{Code: "42P01", Message: `relation "x" does not exist`}
	if reason, ok := unscorableReason(gone); !ok || !strings.Contains(reason, "relation gone") {
		t.Fatalf("42P01: %q %v", reason, ok)
	}
	for _, err := range []error{nil, errors.New("connection refused"),
		&pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}} {
		if reason, ok := unscorableReason(err); ok {
			t.Errorf("%v must stay an error, not unscorable %q", err, reason)
		}
	}
}

func maintenanceShadow(baseline *float64) Decision {
	return Decision{ID: 5, Family: "hygiene", Class: "analyze", Object: "public.t",
		SQL: `ANALYZE public.t`, Prediction: verify.Prediction{Class: verify.ClassAnalyze,
			Method: verify.MethodRule, Metric: verify.MetricModsSinceAnalyze,
			Baseline: baseline}}
}

func TestMaintenanceFactJudgesTheBaselineAgainstTheDetectionRead(t *testing.T) {
	base, after := 5000.0, 0.0
	f := maintenanceFact(maintenanceShadow(&base), &after)
	if !f.Final || f.Verdict != verify.OutcomeImproved || f.Unscorable != "" ||
		f.Evidence["before"] != 5000.0 || f.Evidence["after"] != 0.0 ||
		f.Evidence["metric"] != verify.MetricModsSinceAnalyze {
		t.Fatalf("fact %+v", f)
	}
	j := judge(maintenanceShadow(&base), facts{external: f}, time.Hour, fastOptions())
	if j.Wait || j.Score != ScoreCorrect || j.Source != SourceExternal || !j.Counted {
		t.Fatalf("judgement %+v", j)
	}
}

func TestMaintenanceFactWithoutABaselineIsNeverCounted(t *testing.T) {
	after := 0.0
	for _, d := range []Decision{maintenanceShadow(nil), {Class: "vacuum", Family: "hygiene"}} {
		f := maintenanceFact(d, &after)
		if !f.Final || f.Verdict != verify.OutcomeUnverifiable || f.Reason == "" {
			t.Fatalf("fact %+v", f)
		}
		j := judge(d, facts{external: f}, time.Hour, fastOptions())
		if j.Counted || j.Score != ScoreUnscored {
			t.Fatalf("a decision without a baseline was counted: %+v", j)
		}
	}
	base := 5000.0
	if f := maintenanceFact(maintenanceShadow(&base), nil); f.Verdict !=
		verify.OutcomeUnverifiable || !f.Final {
		t.Fatalf("an unreadable metric must be unverifiable: %+v", f)
	}
}

func TestJudgeUnscorableEvidenceWaitsThenNamesWhy(t *testing.T) {
	o := fastOptions()
	ev := facts{external: &verifiedFact{Unscorable: "relation gone: public.t"}}
	if j := judge(maintenanceShadow(nil), ev, o.Horizon-time.Minute, o); !j.Wait {
		t.Fatalf("before the horizon an unscorable decision waits: %+v", j)
	}
	j := judge(maintenanceShadow(nil), ev, o.Horizon, o)
	if j.Wait || j.Score != ScoreUnscored || j.Source != SourceNone || j.Counted ||
		!strings.Contains(j.Reason, "relation gone") {
		t.Fatalf("at the horizon: %+v", j)
	}
}

// The maintained table is read from the decision's SQL as PostgreSQL
// reads it (quoted names keep their quotes, spaces and case), so a name
// that needs quotes still resolves; a statement with no trailing table
// falls back to the object.
func TestMaintenanceTableReadsTheQuotedName(t *testing.T) {
	cases := []struct{ sql, object, want string }{
		{`VACUUM "public"."orders";`, "public.orders", "public.orders"},
		{`ANALYZE "public"."my t";`, "public.my t", `public."my t"`},
		{`VACUUM "Sales Data"."Big ""Q"" Table"`, "x", `"Sales Data"."Big ""Q"" Table"`},
		{`VACUUM (ANALYZE) Public.Orders ;`, "x", "public.orders"},
		{`VACUUM t`, "x", "t"},
		{`ANALYZE public.t (a, b)`, " public.t ", "public.t"},
		{`VACUUM`, "public.o", "public.o"},
		{``, "", ""},
	}
	for _, c := range cases {
		if got := maintenanceTable(Decision{SQL: c.sql, Object: c.object}); got != c.want {
			t.Errorf("maintenanceTable(%q, %q) = %q, want %q", c.sql, c.object, got, c.want)
		}
	}
}

// A relation name the server cannot even parse (PostgreSQL 14 and 15
// raise it from to_regclass) never resolves for this decision: it is
// unscorable, never an error that stops every later pass.
func TestUnscorableReasonOfAnUnparsableName(t *testing.T) {
	for _, code := range []string{"42602", "42601", "0A000"} {
		err := fmt.Errorf("read: %w", &pgconn.PgError{Code: code, Message: "invalid name syntax"})
		reason, ok := unscorableReason(err)
		if !ok || !strings.Contains(reason, code) ||
			!strings.Contains(reason, "relation name does not resolve") {
			t.Errorf("%s: %q %v", code, reason, ok)
		}
	}
}

// An object that covers expressions is another object, even when its
// kinds and plain columns are the decision's: pg_sage proposes columns only.
func TestStatisticsMatchRefusesExpressions(t *testing.T) {
	c, err := extstats.ParseCreate(`CREATE STATISTICS public.sage_stx_t ON a, b FROM public.t`)
	if err != nil {
		t.Fatal(err)
	}
	kinds, columns := []string{"d", "f", "m"}, []string{"a", "b"}
	if !statisticsMatch(c, kinds, columns, false) {
		t.Fatal("the same columns and kinds without expressions must match")
	}
	if statisticsMatch(c, kinds, columns, true) {
		t.Fatal("an object covering expressions matched a plain-column decision")
	}
}
