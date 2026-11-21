package schema

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// config-04: the precheck classifier and the SQL scanner it rests on.
// No concurrent-access tests here: these functions are pure.

func TestSplitSQL_KeepsQuotedSemicolonsInOneStatement(t *testing.T) {
	cases := map[string][]string{
		"SELECT 'a;b'; SELECT 2":                  {"SELECT 'a;b'", " SELECT 2"},
		"SELECT 'it''s;'; X":                      {"SELECT 'it''s;'", " X"},
		`SELECT "we;ird"; X`:                      {`SELECT "we;ird"`, " X"},
		"DO $$ BEGIN x; y; END $$; X":             {"DO $$ BEGIN x; y; END $$", " X"},
		"DO $do$ a; $$ b; $do$; X":                {"DO $do$ a; $$ b; $do$", " X"},
		"SELECT $1; X":                            {"SELECT $1", " X"},
		"SELECT a$b; X":                           {"SELECT a$b", " X"},
		"-- c; d\nSELECT 1; X":                    {"-- c; d\nSELECT 1", " X"},
		"/* a; /* b; */ c; */ SELECT 1; X":        {"/* a; /* b; */ c; */ SELECT 1", " X"},
		"":                                        {""},
		"SELECT 1;":                               {"SELECT 1", ""},
		"DO $$ unterminated; still one statement": {"DO $$ unterminated; still one statement"},
	}
	for in, want := range cases {
		got := splitSQL(in)
		if !slices.Equal(got, want) {
			t.Errorf("splitSQL(%q) = %q, want %q", in, got, want)
		}
		if joined := strings.Join(got, ";"); joined != in {
			t.Errorf("splitSQL(%q) does not join back: %q", in, joined)
		}
	}
}

func TestSplitTopLevel_IgnoresCommasInParensAndQuotes(t *testing.T) {
	got := splitTopLevel("ADD a numeric(10,2), ADD b text DEFAULT 'x,y', ADD c", ',')
	want := []string{"ADD a numeric(10,2)", " ADD b text DEFAULT 'x,y'", " ADD c"}
	if !slices.Equal(got, want) {
		t.Fatalf("splitTopLevel = %q, want %q", got, want)
	}
}

func TestStripSQLComments_KeepsQuotedText(t *testing.T) {
	got := stripSQLComments("/* c */ALTER -- x\nTABLE 'a -- b' \"/*q*/\"")
	want := " ALTER  TABLE 'a -- b' \"/*q*/\""
	if got != want {
		t.Fatalf("stripSQLComments = %q, want %q", got, want)
	}
}

// classifyCases are statements with the shape and checks the classifier
// must give them.
var classifyCases = []struct {
	name, sql string
	shape     stmtShape
	checks    []catalogCheck
}{
	{"one column", "\nALTER TABLE sage.action_log\n ADD COLUMN IF NOT EXISTS a text",
		shapeColumns, []catalogCheck{{"column", "sage.action_log", "a"}}},
	{"columns with commas in types", "ALTER TABLE IF EXISTS ONLY sage.T ADD COLUMN " +
		"IF NOT EXISTS A numeric(10,2) DEFAULT 0, ADD IF NOT EXISTS b text " +
		"CHECK (b IN ('x', 'y'))", shapeColumns, []catalogCheck{
		{"column", "sage.t", "a"}, {"column", "sage.t", "b"}}},
	{"comment first", "/* pg_sage */ -- why\nALTER TABLE sage.x ADD COLUMN IF NOT " +
		"EXISTS c int", shapeColumns, []catalogCheck{{"column", "sage.x", "c"}}},
	{"mixed clauses", "ALTER TABLE sage.x ADD COLUMN IF NOT EXISTS c int, " +
		"ALTER COLUMN d SET NOT NULL", shapeUnknown, nil},
	{"plain add column", "ALTER TABLE sage.x ADD COLUMN c int", shapeUnknown, nil},
	{"unqualified table", "ALTER TABLE x ADD COLUMN IF NOT EXISTS c int",
		shapeUnknown, nil},
	{"other schema", "ALTER TABLE public.x ADD COLUMN IF NOT EXISTS c int",
		shapeUnknown, nil},
	{"quoted column", `ALTER TABLE sage.x ADD COLUMN IF NOT EXISTS "C" int`,
		shapeUnknown, nil},
	{"drop constraint", "ALTER TABLE sage.x DROP CONSTRAINT IF EXISTS c",
		shapeUnknown, nil},
	{"index", "CREATE INDEX IF NOT EXISTS idx_a ON sage.t (a)", shapeIndex,
		[]catalogCheck{{"index", "sage.idx_a", ""}}},
	{"unique index using", "CREATE UNIQUE INDEX IF NOT EXISTS Idx_B\n ON ONLY " +
		"sage.t USING gin (b)", shapeIndex, []catalogCheck{{"index", "sage.idx_b", ""}}},
	{"index without if not exists", "CREATE INDEX idx_a ON sage.t (a)",
		shapeUnknown, nil},
	{"index on another schema", "CREATE INDEX IF NOT EXISTS idx_a ON public.t (a)",
		shapeUnknown, nil},
	{"concurrent index", "CREATE INDEX CONCURRENTLY IF NOT EXISTS i ON sage.t (a)",
		shapeUnknown, nil},
	{"table", "CREATE TABLE IF NOT EXISTS sage.t (id int)", shapeRelation,
		[]catalogCheck{{"relation", "sage.t", ""}}},
	{"sequence", "CREATE SEQUENCE IF NOT EXISTS sage.s START 1", shapeRelation,
		[]catalogCheck{{"relation", "sage.s", ""}}},
	{"table without if not exists", "CREATE TABLE sage.t (id int)", shapeUnknown, nil},
	{"set local", "SET LOCAL lock_timeout = '5s'", shapeSetting, nil},
	{"set to", "SET statement_timeout TO 0", shapeSetting, nil},
	{"do block", "DO $$ BEGIN NULL; END $$", shapeEveryBoot, nil},
	{"function", "CREATE OR REPLACE FUNCTION sage.f() RETURNS int", shapeEveryBoot, nil},
	{"view", "CREATE OR REPLACE VIEW sage.v AS SELECT 1", shapeEveryBoot, nil},
	{"update", "UPDATE sage.t SET a = 1", shapeEveryBoot, nil},
	{"insert", "INSERT INTO sage.t VALUES (1)", shapeEveryBoot, nil},
	{"delete", "DELETE FROM sage.t WHERE false", shapeEveryBoot, nil},
	{"with", "WITH r AS (SELECT 1) DELETE FROM sage.t", shapeEveryBoot, nil},
	{"update elsewhere", "UPDATE public.t SET a = 1", shapeUnknown, nil},
	{"drop index", "DROP INDEX IF EXISTS sage.i", shapeUnknown, nil},
	{"create trigger", "CREATE TRIGGER t BEFORE UPDATE ON sage.t", shapeUnknown, nil},
	{"garbage", "this is not sql", shapeUnknown, nil},
}

func TestClassifyStatement_Shapes(t *testing.T) {
	for _, tc := range classifyCases {
		got := classifyStatement(tc.sql)
		if got.shape != tc.shape || !slices.Equal(got.checks, tc.checks) {
			t.Errorf("%s: classifyStatement(%q) = shape %d checks %v, want %d %v",
				tc.name, tc.sql, got.shape, got.checks, tc.shape, tc.checks)
		}
		if got.sql != tc.sql {
			t.Errorf("%s: statement text changed to %q", tc.name, got.sql)
		}
	}
}

func TestClassifyStep_DropsEmptyStatements(t *testing.T) {
	got := classifyStep("\n-- only a comment\n; SET LOCAL a = 'b'; /* c */ ;\n")
	if len(got) != 1 || got[0].shape != shapeSetting {
		t.Fatalf("classifyStep = %+v, want the one setting", got)
	}
	if classifyStep("") != nil || classifyStep("  ;\n; ") != nil {
		t.Fatal("an empty step classified to statements")
	}
}

func TestMarkDone_ADoneStatementNeedsEveryFact(t *testing.T) {
	stmts := classifyStep(`SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.t ADD COLUMN IF NOT EXISTS a int, ADD COLUMN IF NOT EXISTS b int;
CREATE INDEX IF NOT EXISTS i ON sage.t (a);
UPDATE sage.t SET a = 0 WHERE a IS NULL`)
	cases := []struct {
		name    string
		present []bool
		done    []bool
		work    bool
	}{
		{"nothing present", []bool{false, false, false},
			[]bool{false, false, false, false}, true},
		{"one column missing keeps the whole ALTER", []bool{true, false, true},
			[]bool{false, false, true, false}, true},
		{"everything present still leaves the backfill", []bool{true, true, true},
			[]bool{false, true, true, false}, true},
	}
	for _, tc := range cases {
		db := &fakeStepDB{answers: [][]bool{tc.present}}
		if err := markDone(context.Background(), db, stmts); err != nil {
			t.Fatalf("%s: markDone: %v", tc.name, err)
		}
		for i, s := range stmts {
			if s.done != tc.done[i] {
				t.Errorf("%s: statement %d done = %t, want %t", tc.name, i, s.done,
					tc.done[i])
			}
		}
		if got := pendingWork(stmts); got != tc.work {
			t.Errorf("%s: pendingWork = %t, want %t", tc.name, got, tc.work)
		}
	}
}

func TestMarkDone_AShortAnswerIsAnError(t *testing.T) {
	stmts := classifyStep("CREATE INDEX IF NOT EXISTS i ON sage.t (a); " +
		"CREATE INDEX IF NOT EXISTS j ON sage.t (b)")
	for _, answer := range [][]bool{{true}, {}, {true, true, true}} {
		db := &fakeStepDB{answers: [][]bool{answer}}
		err := markDone(context.Background(), db, stmts)
		if err == nil || !strings.Contains(err.Error(), "of 2 facts") {
			t.Errorf("answer %v: err = %v, want a count mismatch", answer, err)
		}
	}
}

func TestMarkDone_NoFactsAsksNothing(t *testing.T) {
	db := &fakeStepDB{}
	stmts := classifyStep("DO $$ BEGIN NULL; END $$; SET LOCAL a = 'b'")
	if err := markDone(context.Background(), db, stmts); err != nil || db.queries != 0 {
		t.Fatalf("markDone: err %v after %d queries, want no query", err, db.queries)
	}
	if !pendingWork(stmts) {
		t.Fatal("a DO block is not pending work")
	}
	if pendingWork(classifyStep("SET LOCAL a = 'b'")) || pendingWork(nil) {
		t.Fatal("settings alone, or nothing, counted as work")
	}
}

// Invalid input: every statement of every shipped step has a shape the
// classifier knows, unless its step is marked alwaysRun. A new constant
// with an unknown shape fails here until it is reshaped or reviewed.
func TestMigrationSteps_EveryStatementHasAKnownShape(t *testing.T) {
	steps := append(migrationSteps(),
		migrationStep{name: "ddlConfigColumns", sql: ddlConfigColumns},
		migrationStep{name: "ddlConfigAudit", sql: ddlConfigAudit},
		migrationStep{name: "ddlIncidentLifecycle", sql: ddlIncidentLifecycle},
		migrationStep{name: "auditchain.MigrationSQL", sql: auditchain.MigrationSQL()})
	for _, step := range steps {
		stmts := classifyStep(step.sql)
		if len(stmts) == 0 {
			t.Errorf("step %s has no statements", step.name)
		}
		unknown := 0
		for _, s := range stmts {
			if s.shape == shapeUnknown {
				unknown++
				if !step.alwaysRun {
					t.Errorf("step %s: statement of unknown shape (reshape it or mark "+
						"the step alwaysRun):\n%s", step.name, strings.TrimSpace(s.sql))
				}
			}
		}
		if step.alwaysRun && unknown == 0 {
			t.Errorf("step %s is marked alwaysRun but every statement is known", step.name)
		}
	}
}

func TestMigrationSteps_NamesAreUniqueAndSQLSplitsLosslessly(t *testing.T) {
	seen := map[string]bool{}
	for _, step := range migrationSteps() {
		if step.name == "" || seen[step.name] {
			t.Errorf("step name %q is empty or repeated", step.name)
		}
		seen[step.name] = true
		if strings.Join(splitSQL(step.sql), ";") != step.sql {
			t.Errorf("step %s does not split losslessly", step.name)
		}
	}
	if len(seen) < 70 {
		t.Fatalf("only %d steps: the list lost entries", len(seen))
	}
}

func TestMigrationStatements_AreTheStepsInOrder(t *testing.T) {
	steps, stmts := migrationSteps(), migrationStatements()
	if len(steps) != len(stmts) {
		t.Fatalf("%d steps, %d statements", len(steps), len(stmts))
	}
	for i := range steps {
		if steps[i].sql != stmts[i] {
			t.Fatalf("statement %d is not step %s", i, steps[i].name)
		}
	}
	if steps[0].name != "ddlActionLogApprovalCols" ||
		steps[len(steps)-1].name != "ddlNotificationQuietAndMutes" {
		t.Fatalf("order changed: first %s, last %s", steps[0].name, steps[len(steps)-1].name)
	}
}

// indexOfStep is the position of the step named name in steps, or -1.
func indexOfStep(steps []namedStep, name string) int {
	for i, step := range steps {
		if step[0] == name {
			return i
		}
	}
	return -1
}
