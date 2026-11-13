package migration

import (
	"context"
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// env-cgo-b: the ADD COLUMN ... DEFAULT rewrite rule must read the
// function a default calls the way PostgreSQL does, on both classifier
// paths. The parse-tree mapping sees the real name (a quoted name keeps
// its case, a schema other than pg_catalog can name any function); the
// regex rule must never be less strict than it (migration-04 CHECK-03).

// No concurrent access tests here: isVolatileDefault and the mapper are
// pure functions; TestClassifyStatementsConcurrentCallers covers the batch.
// State transitions: do not apply (classification is stateless).

type defaultCallCase struct {
	expr     string   // the DEFAULT expression as written
	funcname []string // the FuncCall name the parser gives for expr
	rewrites bool     // PostgreSQL 11+ rewrites the table for this default
}

var defaultCallCases = []defaultCallCase{
	{`now()`, []string{"now"}, false},
	{`pg_catalog.now()`, []string{"pg_catalog", "now"}, false},
	{`PG_CATALOG.NOW()`, []string{"pg_catalog", "now"}, false},
	{`"pg_catalog"."now"()`, []string{"pg_catalog", "now"}, false},
	{`"now"()`, []string{"now"}, false},
	{`random()`, []string{"random"}, true},
	{`"random"()`, []string{"random"}, true},
	{`"NOW"()`, []string{"NOW"}, true},
	{`app.now()`, []string{"app", "now"}, true},
	{`"app".now()`, []string{"app", "now"}, true},
	{`app . "now" ()`, []string{"app", "now"}, true},
	{`nextval('s')`, []string{"nextval"}, true},
	{`pg_catalog.clock_timestamp()`, []string{"pg_catalog", "clock_timestamp"}, true},
}

func addDefaultSQL(expr string) string {
	return "ALTER TABLE app.t ADD COLUMN c timestamptz DEFAULT " + expr
}

func hasRewriteRule(t *testing.T, cs []DDLClassification) bool {
	t.Helper()
	found := false
	for _, c := range cs {
		if c.RuleID != "ddl_add_column_volatile_default" {
			continue
		}
		if c.LockLevel != lockAccessExclusive || c.TableName != "t" {
			t.Fatalf("rewrite rule %+v, want ACCESS EXCLUSIVE on t", c)
		}
		found = true
	}
	return found
}

// The regex rule flags a quoted or schema-qualified volatile call, which
// it read as a constant before env-cgo-b.
func TestRegexAddColumnDefaultReadsTheCalledFunction(t *testing.T) {
	re := NewRegexClassifier()
	for _, tc := range defaultCallCases {
		got := hasRewriteRule(t, re.Classify(addDefaultSQL(tc.expr), 170000))
		if got != tc.rewrites {
			t.Errorf("regex DEFAULT %s: rewrite rule %v, want %v", tc.expr, got, tc.rewrites)
		}
	}
}

func funcCallNode(names ...string) *pg_query.Node {
	fc := &pg_query.FuncCall{}
	for _, n := range names {
		fc.Funcname = append(fc.Funcname, strNode(n))
	}
	return &pg_query.Node{Node: &pg_query.Node_FuncCall{FuncCall: fc}}
}

// The parse-tree mapping and the regex rule agree on every spelling, so
// neither path is less strict than the other.
func TestASTDefaultCallMatchesRegexStrictness(t *testing.T) {
	re := NewRegexClassifier()
	for _, tc := range defaultCallCases {
		def := columnDefNode(typeName("pg_catalog", "timestamptz"),
			&pg_query.Constraint{Contype: pg_query.ConstrType_CONSTR_DEFAULT,
				RawExpr: funcCallNode(tc.funcname...)})
		found, known := testMapper.statement(addColumnStmt(def))
		if !known {
			t.Fatalf("tree for DEFAULT %s unrecognized", tc.expr)
		}
		ast := hasRewriteRule(t, found)
		regex := hasRewriteRule(t, re.Classify(addDefaultSQL(tc.expr), 170000))
		if ast != tc.rewrites || regex != tc.rewrites {
			t.Errorf("DEFAULT %s: parse tree %v, regex %v, want %v",
				tc.expr, ast, regex, tc.rewrites)
		}
	}
}

// Through the one entry point the rewrite is reported on every build.
// Without the parser a default that is neither a rewrite rule match nor
// proven by the additive allowlist stays unrecognized (fail closed).
func TestClassifyStatementsDefaultCallOnThisBuild(t *testing.T) {
	for _, tc := range defaultCallCases {
		res, err := ClassifyStatements(context.Background(), nil, addDefaultSQL(tc.expr), 170000)
		if err != nil || len(res.Statements) != 1 {
			t.Fatalf("DEFAULT %s: %+v %v", tc.expr, res, err)
		}
		if got := hasRewriteRule(t, res.All()); got != tc.rewrites {
			t.Errorf("DEFAULT %s: rewrite rule %v, want %v", tc.expr, got, tc.rewrites)
		}
		want := StatementKnown
		if !parserAvailable && !tc.rewrites && !additiveDefaults[tc.expr] {
			want = StatementUnrecognized
		}
		if st := res.Statements[0].Status; st != want {
			t.Errorf("DEFAULT %s: status %s, want %s (parser %v)", tc.expr, st, want,
				parserAvailable)
		}
	}
}

// Nil/empty and boundary inputs of the call reader: no call is no
// rewrite; an immutable call followed by a volatile one is a rewrite.
func TestIsVolatileDefaultEdges(t *testing.T) {
	cases := map[string]bool{
		"":                                 false,
		"'active'":                         false,
		"0":                                false,
		`now() + random() * interval '1s'`: true,
		`random() + now()`:                 true,
		`app.`:                             false,
		`""()`:                             true,
		`"a""b"()`:                         true,
		`app.now`:                          false,
	}
	for expr, want := range cases {
		if got := isVolatileDefault(expr); got != want {
			t.Errorf("isVolatileDefault(%q) = %v, want %v", expr, got, want)
		}
	}
}

// additiveDefaults are the cases the additive allowlist proves today:
// unquoted now(), bare or qualified by pg_catalog.
var additiveDefaults = map[string]bool{
	"now()": true, "pg_catalog.now()": true, "PG_CATALOG.NOW()": true,
}

// The additive allowlist keeps refusing every quoted or qualified call
// (env-cgo-b must not widen it).
func TestAdditiveAllowlistStillRefusesQuotedAndQualifiedCalls(t *testing.T) {
	for _, tc := range defaultCallCases {
		sub := "ADD COLUMN c timestamptz DEFAULT " + tc.expr
		want := additiveDefaults[tc.expr]
		if got := addColumnAdditive(sub); got != want {
			t.Errorf("addColumnAdditive(%q) = %v, want %v", sub, got, want)
		}
	}
}
