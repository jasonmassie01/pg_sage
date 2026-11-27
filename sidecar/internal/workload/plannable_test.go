package workload

import "testing"

type plannableCase struct {
	query        string
	ok, readOnly bool
}

var plannableCases = []plannableCase{
	// Read-only queries.
	{"SELECT 1", true, true},
	{"/* c */ SELECT 1", true, true},
	{"-- c\nselect * from users", true, true},
	{"/* a */ /* b */\n\tSELECT 1", true, true},
	{"/* a; b */ SELECT 1", true, true},
	{"/* pg_sage */ SELECT * FROM sage.findings WHERE id = $1", true, true},
	{"VALUES ($1), ($2)", true, true},
	{"TABLE orders", true, true},
	{"WITH cte AS (SELECT 1) SELECT * FROM cte", true, true},
	{"with recursive r(n) AS (SELECT 1) SELECT n FROM r", true, true},
	{"WITH x AS (SELECT 1) SELECT * FROM updates, deleted_rows", true, true},
	{"SELECT * FROM t WHERE id = $1 FOR UPDATE", true, true},
	{"SELECT 1;", true, true},
	{"SELECT 1 ;  \n", true, true},
	// DML: plannable, not read-only.
	{"INSERT INTO foo VALUES (1)", true, false},
	{"UPDATE foo SET x = 1", true, false},
	{"DELETE FROM foo", true, false},
	{"MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN DELETE", true, false},
	{"/* a */ merge into t using s on true when matched then do nothing", true, false},
	{"WITH x AS (SELECT 1) UPDATE t SET a = 1", true, false},
	{"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d", true, false},
	{"WITH i AS (INSERT INTO t VALUES (1) RETURNING id) SELECT id FROM i", true, false},
	// A DML word anywhere in a WITH fails closed to not read-only.
	{"WITH x AS (SELECT 'update' AS w) SELECT * FROM x", true, false},
	// Not plannable.
	{"", false, false},
	{"   \n\t", false, false},
	{"SELECT 1; SELECT 2", false, false},
	{"UPDATE t SET a = 1; DELETE FROM t", false, false},
	// A -- comment ends at CR (as in the PostgreSQL lexer), so the ; after
	// it starts a second statement.
	{"-- c\r; DELETE FROM t\nSELECT 1", false, false},
	{"-- c\rSELECT 1", true, true},
	{"/* unterminated SELECT 1", false, false},
	{"-- only a comment", false, false},
	{"EXPLAIN SELECT 1", false, false},
	{"SET search_path TO public", false, false},
	{"VACUUM foo", false, false},
	{"CREATE INDEX idx ON foo (bar)", false, false},
	{"COPY foo FROM stdin", false, false},
	{"COPY (SELECT 1) TO STDOUT", false, false},
	{"DROP TABLE foo", false, false},
	{"ALTER TABLE foo ADD COLUMN bar int", false, false},
	{"BEGIN", false, false},
	{"COMMIT", false, false},
	{"LOCK TABLE t", false, false},
	{"CALL p($1)", false, false},
	{"DO $$ BEGIN END $$", false, false},
	{"REFRESH MATERIALIZED VIEW daily_totals", false, false},
	{"selected_rows", false, false},
	{"settings_table_select", false, false},
	{"with_x", false, false},
	{"tables", false, false},
}

func TestPlannable(t *testing.T) {
	for _, tc := range plannableCases {
		ok, readOnly := Plannable(tc.query)
		if ok != tc.ok || readOnly != tc.readOnly {
			t.Errorf("Plannable(%q) = (%v, %v), want (%v, %v)", tc.query, ok, readOnly,
				tc.ok, tc.readOnly)
		}
	}
}

// A statement that is read-only is always plannable: no caller may get
// readOnly without ok.
func TestPlannableReadOnlyImpliesOK(t *testing.T) {
	corpus := append(append([]classifyCase{}, classifyCases...), proceduralCases...)
	for _, tc := range append(corpus, monitorCases...) {
		if ok, readOnly := Plannable(tc.query); readOnly && !ok {
			t.Errorf("%s: Plannable(%q) = (false, true)", tc.name, tc.query)
		}
	}
}

// removedCopy is one input of a removed private planner check and the
// answer that copy gave.
type removedCopy struct {
	query string
	want  bool
}

// commentPrefixFixed are the inputs where the autoexplain and hypopg
// copies were wrong: a leading comment made them refuse a plannable query.
var commentPrefixFixed = map[string]bool{
	"/* app */ SELECT * FROM orders":           true,
	"-- report\nWITH x AS (SELECT 1) SELECT 1": true,
}

// Regression lock: Plannable agrees with each removed copy on that copy's
// test inputs, except the documented comment-prefix fix.
func TestPlannableAgreesWithRemovedAutoexplainCopy(t *testing.T) {
	// autoexplain.isExplainable: SELECT/INSERT/UPDATE/DELETE/WITH prefix.
	cases := []removedCopy{
		{"SELECT 1", true}, {"select * from users", true},
		{"INSERT INTO foo VALUES (1)", true}, {"UPDATE foo SET x=1", true},
		{"DELETE FROM foo", true}, {"WITH cte AS (SELECT 1) SELECT * FROM cte", true},
		{"  SELECT 1", true}, {"\tSELECT 1", true}, {"\nSELECT 1", true},
		{"With cte AS (SELECT 1) SELECT * FROM cte", true},
		{"SET search_path TO public", false}, {"VACUUM foo", false},
		{"CREATE INDEX idx ON foo (bar)", false}, {"COPY foo FROM stdin", false},
		{"", false}, {"   ", false}, {"DROP TABLE foo", false},
		{"ALTER TABLE foo ADD COLUMN bar int", false},
		{"/* app */ SELECT * FROM orders", false},
		{"-- report\nWITH x AS (SELECT 1) SELECT 1", false},
	}
	for _, c := range cases {
		ok, _ := Plannable(c.query)
		want := c.want || commentPrefixFixed[c.query]
		if ok != want {
			t.Errorf("Plannable(%q) ok = %v, autoexplain copy said %v (fixed: %v)",
				c.query, ok, c.want, commentPrefixFixed[c.query])
		}
	}
}

func TestPlannableAgreesWithRemovedHypoPGCopy(t *testing.T) {
	// optimizer.isExplainable: SELECT/WITH prefix (the what-if's read-only scope).
	cases := []removedCopy{
		{"WITH cte AS (SELECT 1) SELECT * FROM cte", true}, {"DELETE FROM orders", false},
		{"SELECT 1", true}, {"  select * from t", true},
		{"INSERT INTO t VALUES (1)", false}, {"UPDATE t SET a = 1", false},
		{"DELETE FROM t WHERE id = 1", false}, {"CREATE TABLE t (id int)", false},
		{"", false}, {"   ", false},
		{"/* app */ SELECT * FROM orders", false},
		{"-- report\nWITH x AS (SELECT 1) SELECT 1", false},
	}
	for _, c := range cases {
		ok, readOnly := Plannable(c.query)
		want := c.want || commentPrefixFixed[c.query]
		if (ok && readOnly) != want {
			t.Errorf("Plannable(%q) = (%v, %v), hypopg copy said %v (fixed: %v)",
				c.query, ok, readOnly, c.want, commentPrefixFixed[c.query])
		}
	}
}

func TestPlannableAgreesWithRemovedPerfgateCopy(t *testing.T) {
	// perfgate.Explainable: comment-stripping, MERGE, single statement.
	yes := []string{
		"SELECT 1",
		"/* pg_sage */ SELECT * FROM sage.findings WHERE id = $1",
		"-- leading comment\nWITH x AS (SELECT 1) SELECT * FROM x",
		"  insert into sage.snapshots (data) values ($1)",
		"UPDATE sage.findings SET status = $1",
		"DELETE FROM sage.snapshots WHERE id = ANY($1)",
		"/* a */ /* b */ MERGE INTO sage.x USING y ON true WHEN MATCHED THEN DELETE",
	}
	no := []string{
		"", "BEGIN", "COMMIT", "SET statement_timeout = $1", "SET LOCAL lock_timeout = 1",
		"/* pg_sage */ VACUUM sage.snapshots", "CREATE INDEX CONCURRENTLY x ON y (z)",
		"EXPLAIN SELECT 1", "SELECT 1; SELECT 2", "/* unterminated SELECT 1",
		"DO $$ BEGIN END $$", "LOCK TABLE sage.findings", "COPY sage.x FROM STDIN",
	}
	for _, q := range yes {
		if ok, _ := Plannable(q); !ok {
			t.Errorf("Plannable(%q) not ok; the perfgate copy planned it", q)
		}
	}
	for _, q := range no {
		if ok, _ := Plannable(q); ok {
			t.Errorf("Plannable(%q) ok; the perfgate copy refused it", q)
		}
	}
}

// No concurrent access or error propagation tests: Plannable is a pure
// function over regexps compiled at init and returns no error.
