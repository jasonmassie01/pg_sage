package mutationcensus

import (
	"strings"
	"testing"
)

func TestParseSinksReadsRows(t *testing.T) {
	src := "# comment line\n\n" +
		"internal/a/b.go#F\tsage_schema\twrites sage.x only\r\n" +
		"cmd/x/main.go#T.M\tout_of_gate:operator_cli\tthe operator runs it\n"
	got, err := parseSinks(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []entry{
		{key: "internal/a/b.go#F", class: "sage_schema", note: "writes sage.x only", line: 3},
		{key: "cmd/x/main.go#T.M", class: "out_of_gate:operator_cli",
			note: "the operator runs it", line: 4},
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseSinksEmptyInput(t *testing.T) {
	got, err := parseSinks(strings.NewReader(""))
	if err != nil || len(got) != 0 {
		t.Fatalf("empty input = %v, %v; want no entries and no error", got, err)
	}
}

func TestParseSinksRejectsMalformedLines(t *testing.T) {
	cases := map[string]struct{ src, want string }{
		"two fields": {"internal/a/b.go#F\tgate:analyze_table\n",
			"line 1: want path#Func<TAB>class<TAB>note, got 2"},
		"four fields": {"# c\ninternal/a/b.go#F\ttext\tnote\textra\n",
			"line 2: want path#Func<TAB>class<TAB>note, got 4"},
		"no func": {"internal/a/b.go\ttext\tnote\n", `line 1: key "internal/a/b.go"`},
		"outside tree": {"scripts/x.go#F\ttext\tnote\n",
			`line 1: key "scripts/x.go#F" is not`},
		"empty note": {"internal/a/b.go#F\ttext\t  \n", "line 1: internal/a/b.go#F has no note"},
		"duplicate": {"internal/a/b.go#F\ttext\tn\n\ninternal/a/b.go#F\ttext\tn\n",
			"line 3: internal/a/b.go#F is already listed on line 1"},
	}
	for name, tc := range cases {
		_, err := parseSinks(strings.NewReader(tc.src))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
		}
	}
}

func TestCheckClass(t *testing.T) {
	valid := []string{"sage_schema", "text", "clone_only", "out_of_gate:bootstrap",
		"gate:analyze_table", "gate:guard_grant,guard_revoke"}
	for _, class := range valid {
		if err := checkClass(class); err != nil {
			t.Errorf("checkClass(%q) = %v, want nil", class, err)
		}
	}
	invalid := map[string]string{
		"":                          "unknown class",
		"gate":                      "names no contract",
		"gate:":                     "names no contract",
		"gate:no_such_contract":     `"no_such_contract" is not a contract registered`,
		"gate:analyze_table,":       `"" is not a contract registered`,
		"out_of_gate:":              "needs a snake_case reason",
		"out_of_gate:Bad Reason":    "needs a snake_case reason",
		"text:extra":                "takes no detail",
		"sage_schema:x":             "takes no detail",
		"GATE:analyze_table":        "unknown class",
		"out_of_gate_bootstrap":     "unknown class",
		"gate:analyze_table;DROP x": "is not a contract registered",
	}
	for class, want := range invalid {
		err := checkClass(class)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("checkClass(%q) = %v, want it to contain %q", class, err, want)
		}
	}
}

func TestWriteStatements(t *testing.T) {
	sinks := map[string]string{
		"delete":        "DELETE FROM public.t",
		"lower":         "delete from t",
		"update set":    "UPDATE t SET a = 1",
		"multi":         "SELECT 1; TRUNCATE public.t",
		"grant":         "GRANT SELECT ON public.t TO r",
		"grant role":    "GRANT pg_monitor TO sage_agent_x",
		"analyze bare":  "ANALYZE",
		"refresh":       "REFRESH MATERIALIZED VIEW public.mv",
		"plpgsql":       "DO $$ BEGIN IF true THEN DROP TABLE public.t; END IF; END $$",
		"merge":         "MERGE INTO public.t USING s ON true WHEN MATCHED THEN DELETE",
		"extension":     "CREATE EXTENSION IF NOT EXISTS pg_stat_statements",
		"sage in later": "INSERT INTO t SELECT * FROM sage.x",
	}
	notSinks := map[string]string{
		"select":        "SELECT a FROM t",
		"update ident":  "SELECT updated_at FROM t",
		"update no set": "SELECT 1 WHERE 'UPDATE' = $1",
		"created":       "SELECT created FROM t",
		"sage insert":   "INSERT INTO sage.x VALUES (1)",
		"sage quoted":   `UPDATE "sage".x SET a = 1`,
		"sage schema":   "CREATE SCHEMA IF NOT EXISTS sage",
		"grant sage":    "GRANT USAGE ON SCHEMA sage TO pg_monitor",
		"sage index":    "CREATE INDEX IF NOT EXISTS i ON sage.x (a)",
		"sage function": "CREATE OR REPLACE FUNCTION sage.f() RETURNS int AS $$ SELECT 1 $$",
		"empty":         "",
		"comment":       "/* DROP TABLE t */ SELECT 1",
	}
	checkWrites(t, sinks, notSinks)
}

// Lists and the replication and WAL functions: a DROP or GRANT that names
// a sage target first must not hide the user target after it.
func TestWriteStatementTargetLists(t *testing.T) {
	sinks := map[string]string{
		"drop list":     "DROP TABLE IF EXISTS sage.a, public.b CASCADE",
		"truncate list": "TRUNCATE sage.a, b",
		"grant list":    "GRANT SELECT ON sage.a, public.b TO r",
		"schema list":   "GRANT USAGE ON SCHEMA sage, public TO r",
		"drop schemas":  "DROP SCHEMA IF EXISTS sage, public CASCADE",
		"vacuum list":   "VACUUM (ANALYZE, VERBOSE) sage.a, public.b",
		"slot drop":     "SELECT pg_drop_replication_slot($1)",
		"slot create":   "SELECT * FROM pg_create_logical_replication_slot($1, 'pgoutput')",
		"slot advance":  "SELECT pg_replication_slot_advance($1, $2)",
		"switch wal":    "SELECT pg_switch_wal()",
		"promote":       "SELECT pg_promote()",
		"restore point": "SELECT pg_create_restore_point('x')",
	}
	notSinks := map[string]string{
		"sage drop list": "DROP TABLE IF EXISTS sage.a, \"sage\".b CASCADE",
		"sage vacuum":    "VACUUM (ANALYZE, VERBOSE) sage.a, sage.b",
		"sage func drop": "DROP FUNCTION IF EXISTS sage.f(int, text)",
		"sage alter":     "ALTER TABLE sage.x ADD COLUMN a int, ADD COLUMN b int",
		"slot read":      "SELECT slot_name FROM pg_replication_slots",
	}
	checkWrites(t, sinks, notSinks)
}

func checkWrites(t *testing.T, sinks, notSinks map[string]string) {
	t.Helper()
	for name, sql := range sinks {
		if got := writeStatements(sql); len(got) == 0 {
			t.Errorf("%s: %q is not reported as a write", name, sql)
		}
	}
	for name, sql := range notSinks {
		if got := writeStatements(sql); len(got) != 0 {
			t.Errorf("%s: %q reported as writes %q", name, sql, got)
		}
	}
}
