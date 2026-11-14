package migration

import (
	"context"
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// env-cgo-b review: the regex ADD COLUMN rewrite rule read one DEFAULT
// per statement (the last, through a greedy match) and gave up when the
// first ADD was ADD CONSTRAINT, so a volatile default in an earlier or
// later clause of a multi-clause ALTER TABLE was missed. The parse-tree
// mapping checks every subcommand; the regex rule must not be less strict.

// No concurrent access tests here: the rule is a pure function of the
// statement; TestClassifyStatementsConcurrentCallers covers the batch.

var multiClauseRewrites = map[string]bool{
	"ALTER TABLE app.t ADD COLUMN a int DEFAULT random(), ADD COLUMN b int DEFAULT 0": true,
	"ALTER TABLE app.t ADD COLUMN a int DEFAULT 0, ADD COLUMN b int DEFAULT random()": true,
	"ALTER TABLE app.t ADD CONSTRAINT k CHECK (x > 0) NOT VALID, " +
		"ADD COLUMN y timestamptz DEFAULT random()": true,
	"ALTER TABLE app.t ADD CONSTRAINT k CHECK (x > 0) NOT VALID, ADD COLUMN y serial":      true,
	"ALTER TABLE app.t ADD COLUMN a int DEFAULT 0, ADD COLUMN b text DEFAULT 'x'":          false,
	"ALTER TABLE app.t ADD COLUMN a int DEFAULT 0, ADD COLUMN b timestamptz DEFAULT now()": false,
	"ALTER TABLE app.t ADD CONSTRAINT k CHECK (x > 0) NOT VALID, ADD COLUMN y int":         false,
}

func TestRegexAddColumnDefaultReadsEveryClause(t *testing.T) {
	re := NewRegexClassifier()
	for sql, want := range multiClauseRewrites {
		if got := hasRewriteRule(t, re.Classify(sql, 170000)); got != want {
			t.Errorf("regex %q: rewrite rule %v, want %v", sql, got, want)
		}
	}
}

// A volatile default in the first of two ADD COLUMN clauses, next to an
// FK: the statement is a table rewrite under ACCESS EXCLUSIVE on every
// build, not only the FK's SHARE ROW EXCLUSIVE.
func TestClassifyStatementsMultiClauseVolatileDefault(t *testing.T) {
	sql := "ALTER TABLE app.t ADD COLUMN a int DEFAULT random(), ADD COLUMN b int DEFAULT 0, " +
		"ADD CONSTRAINT f FOREIGN KEY (x) REFERENCES app.p (id)"
	res, err := ClassifyStatements(context.Background(), nil, sql, 170000)
	if err != nil || len(res.Statements) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if !hasRewriteRule(t, res.All()) {
		t.Fatalf("no rewrite rule: %+v", res.All())
	}
	if !hasRule(res.All(), "ddl_missing_lock_timeout") {
		t.Errorf("ACCESS EXCLUSIVE without lock_timeout not annotated: %+v", res.All())
	}
}

func hasRule(cs []DDLClassification, id string) bool {
	for _, c := range cs {
		if c.RuleID == id {
			return true
		}
	}
	return false
}

// The parse-tree mapping flags the same two-clause statement, so the two
// paths agree (CHECK-03).
func TestASTMultiClauseDefaultMatchesRegex(t *testing.T) {
	def := func(fn ...string) *pg_query.Node {
		var expr *pg_query.Node
		if len(fn) > 0 {
			expr = funcCallNode(fn...)
		} else {
			expr = pg_query.MakeAConstIntNode(0, 0)
		}
		return columnDefNode(typeName("pg_catalog", "int4"),
			&pg_query.Constraint{Contype: pg_query.ConstrType_CONSTR_DEFAULT, RawExpr: expr})
	}
	stmt := alterStmt(cmd(pg_query.AlterTableType_AT_AddColumn, def("random")),
		cmd(pg_query.AlterTableType_AT_AddColumn, def()))
	found, known := testMapper.statement(stmt)
	if !known || !hasRewriteRule(t, found) {
		t.Fatalf("parse tree: known %v, rules %+v; want a rewrite", known, found)
	}
	sql := "ALTER TABLE app.t ADD COLUMN a int DEFAULT random(), ADD COLUMN b int DEFAULT 0"
	if !hasRewriteRule(t, NewRegexClassifier().Classify(sql, 170000)) {
		t.Errorf("regex path less strict than the parse tree for %q", sql)
	}
}
