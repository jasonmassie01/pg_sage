//go:build cgo

package migration

import (
	"context"
	"strings"
	"testing"
)

// env-cgo-b: these drive the real parser (libpg_query, PG 17 grammar).

// No state transition tests: classification is stateless.
// Concurrency: TestASTStatementConcurrentCallers and
// TestClassifyStatementsConcurrentCallers cover it, run with -race.

// Invalid input: a statement that does not parse is unrecognized and
// still gets the regex rules (fail closed with cgo too).
func TestASTUnparseableStatementFailsClosed(t *testing.T) {
	for _, sql := range []string{"ALTER TABLE t ADD COLUMN", "CREATE INDEX ON",
		"ALTER TABLE t ALTER COLUMN c TYPE"} {
		if _, known := astStatement(sql, 170000); known {
			t.Fatalf("%q parsed as a known form", sql)
		}
		res, err := ClassifyStatements(context.Background(), nil, sql, 170000)
		if err != nil || res.Unrecognized() != 1 || res.Degraded {
			t.Fatalf("%q: %+v %v, want one unrecognized statement", sql, res, err)
		}
	}
}

// Nil/empty: empty and comment-only text gives no statement and no panic.
func TestASTEmptyAndCommentOnlyText(t *testing.T) {
	if found, known := astStatement("", 170000); known || len(found) != 0 {
		t.Fatalf("empty statement: %v %v", ruleIDs(found), known)
	}
	for _, sql := range []string{"", "/* only */", "-- only\n", " ; ; "} {
		res, err := ClassifyStatements(context.Background(), nil, sql, 170000)
		if err != nil || len(res.Statements) != 0 || res.All() != nil {
			t.Fatalf("%q: %+v %v, want no statements", sql, res, err)
		}
	}
}

// Boundary: PG11+ rewrites only for a volatile default.
func TestASTAddColumnDefaultVolatility(t *testing.T) {
	cases := map[string]bool{
		"random()": true, "clock_timestamp()": true, "gen_random_uuid()": true,
		`"random"()`: true, "app.now()": true, `"NOW"()`: true,
		"now()": false, "pg_catalog.now()": false, "'x'": false, "0": false,
		"current_timestamp": false, `"now"()`: false,
	}
	for expr, want := range cases {
		found, known := astStatement(addDefaultSQL(expr), 170000)
		if !known {
			t.Fatalf("DEFAULT %s unrecognized", expr)
		}
		if got := hasRewriteRule(t, found); got != want {
			t.Errorf("DEFAULT %s: rewrite rule %v, want %v", expr, got, want)
		}
	}
}

// Boundary: CREATE INDEX CONCURRENTLY inside a transaction block is not
// a lock hazard (PostgreSQL refuses it before taking any lock); each
// statement of the block is a known form.
func TestASTConcurrentIndexInsideTransactionBlock(t *testing.T) {
	sql := "BEGIN; CREATE INDEX CONCURRENTLY i ON app.t (c); COMMIT"
	res, err := ClassifyStatements(context.Background(), nil, sql, 170000)
	if err != nil || len(res.Statements) != 3 || res.Unrecognized() != 0 {
		t.Fatalf("%+v %v, want three known statements", res, err)
	}
	for _, c := range res.All() {
		if c.RuleID == "ddl_index_not_concurrent" {
			t.Fatalf("CONCURRENTLY reported as a blocking build: %+v", c)
		}
	}
}

// Error propagation: the parser's own message reaches the statement's
// note (the lint result's reason) unchanged, after the fixed text.
func TestReviewNoteCarriesTheParserRefusal(t *testing.T) {
	for _, sql := range []string{"ALTER TABLE t ADD COLUMN", "CREATE INDEX ON",
		"ALTER TABLE t ADD COLUMN c int DEFAULT 1 garbage"} {
		_, perr := parseStatement(sql)
		if perr == nil || perr.Error() == "" {
			t.Fatalf("%q: the parser accepted it", sql)
		}
		st := reviewOne(t, sql)
		if st.Status != StatementUnrecognized || st.Verdict != VerdictReview {
			t.Fatalf("%q: status %s verdict %s, want unrecognized review", sql,
				st.Status, st.Verdict)
		}
		if !strings.HasPrefix(st.Note, "No DDL safety rule matches") {
			t.Errorf("%q: note %q lost the fixed text", sql, st.Note)
		}
		if !strings.Contains(st.Note, refusalPrefix+perr.Error()+".") {
			t.Errorf("%q: note %q does not carry the parser's %q", sql, st.Note, perr)
		}
	}
}

// refusalPrefix is the note's text before the parser's own message.
const refusalPrefix = "The SQL parser (PostgreSQL 17 grammar) refused it: "

// Boundary (parser version): syntax only PostgreSQL 18 accepts is refused
// by the PG 17 grammar; it fails closed and the note names the grammar,
// so a valid PG 18 statement does not read as the author's typo.
func TestReviewNoteNamesTheGrammarForNewerSyntax(t *testing.T) {
	sql := "ALTER TABLE t ADD COLUMN c int GENERATED ALWAYS AS (a * 2) VIRTUAL"
	if _, known := astStatement(sql, 180000); known {
		t.Fatalf("%q is a known form to the PG 17 grammar", sql)
	}
	st := reviewOne(t, sql)
	if st.Status != StatementUnrecognized || st.Verdict != VerdictReview {
		t.Fatalf("status %s verdict %s, want unrecognized review", st.Status, st.Verdict)
	}
	want := refusalPrefix + `syntax error at or near "VIRTUAL".`
	if !strings.HasSuffix(st.Note, want) {
		t.Fatalf("note %q, want it to end %q", st.Note, want)
	}
}

// A statement that parses names no refusal: unmapped, or a known form.
func TestReviewNoteNamesNoRefusalForParsedText(t *testing.T) {
	for _, sql := range []string{"GRANT SELECT ON t TO r",
		"CREATE INDEX CONCURRENTLY i ON t (c)", "ALTER TABLE t DROP COLUMN c"} {
		if st := reviewOne(t, sql); strings.Contains(st.Note, "refused it") {
			t.Errorf("%q parses, but its note says %q", sql, st.Note)
		}
	}
	if got := parseRefusal("ALTER TABLE t DROP COLUMN c"); got != "" {
		t.Fatalf("parseRefusal of valid SQL = %q, want empty", got)
	}
	if got := parseRefusal(""); got != "" {
		t.Fatalf("parseRefusal of empty text = %q, want empty", got)
	}
}

// The refusal is of the redacted statement: a literal never reaches it.
func TestReviewParserRefusalKeepsLiteralsRedacted(t *testing.T) {
	st := reviewOne(t, "ALTER TABLE t ADD COLUMN c text DEFAULT 'hunter2' (")
	if !strings.Contains(st.Note, refusalPrefix) {
		t.Fatalf("note %q lacks the refusal", st.Note)
	}
	if strings.Contains(st.Note, "hunter2") || strings.Contains(st.Statement, "hunter2") {
		t.Fatalf("a literal leaked: statement %q note %q", st.Statement, st.Note)
	}
}

func reviewOne(t *testing.T, sql string) StatementReview {
	t.Helper()
	rep, err := Review(context.Background(), nil, sql, ReviewOptions{})
	if err != nil || len(rep.Statements) != 1 {
		t.Fatalf("Review(%q): %+v %v, want one statement", sql, rep, err)
	}
	return rep.Statements[0]
}
