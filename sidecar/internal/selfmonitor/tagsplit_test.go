package selfmonitor

import (
	"strings"
	"testing"
	"unicode"

	"github.com/jackc/pgx/v5/pgproto3"
)

// A simple Query message may carry several statements (pg_sage's schema
// bootstrap sends its DDL that way, as one implicit transaction).
// pg_stat_statements stores each statement's own text, from its first
// token on every major (PostgreSQL 14-18), so a tag on the first statement
// only marks that one: every statement gets its own tag. Text inside
// literals, quoted identifiers, dollar quotes and comments is never split
// or changed.
func TestTagConn_EveryStatementOfAQueryIsTagged(t *testing.T) {
	cases := map[string]string{
		"SELECT 1; SELECT 2": "SELECT /* pg_sage */ 1; SELECT /* pg_sage */ 2",
		"CREATE TABLE a (x int);\nCREATE INDEX i ON a (x);\n": "CREATE /* pg_sage */ TABLE " +
			"a (x int);\nCREATE /* pg_sage */ INDEX i ON a (x);\n",
		"SELECT 1; /* pg_sage */ SELECT 2": "SELECT /* pg_sage */ 1; SELECT /* pg_sage */ 2",
		"SELECT 1;\n/* pg_sage sre:x v1 */ SELECT 2": "SELECT /* pg_sage */ 1;\nSELECT " +
			"/* pg_sage sre:x v1 */ 2",
		"SELECT 1; SELECT /* pg_sage */ 2": "SELECT /* pg_sage */ 1; SELECT /* pg_sage */ 2",
		"SELECT 'a; DROP'; SELECT 2": "SELECT /* pg_sage */ 'a; DROP'; SELECT " +
			"/* pg_sage */ 2",
		"SELECT 'it''s; x'; SELECT 2": "SELECT /* pg_sage */ 'it''s; x'; SELECT " +
			"/* pg_sage */ 2",
		`SELECT "a;b" FROM t; SELECT 2`: `SELECT /* pg_sage */ "a;b" FROM t; SELECT ` +
			"/* pg_sage */ 2",
		`SELECT "a"";b" FROM t; SELECT 2`: `SELECT /* pg_sage */ "a"";b" FROM t; SELECT ` +
			"/* pg_sage */ 2",
		"DO $$ BEGIN PERFORM 1; END $$; SELECT 2": "DO /* pg_sage */ $$ BEGIN PERFORM 1; " +
			"END $$; SELECT /* pg_sage */ 2",
		"DO $fn$ BEGIN PERFORM '$$;'; END $fn$; SELECT 2": "DO /* pg_sage */ $fn$ BEGIN " +
			"PERFORM '$$;'; END $fn$; SELECT /* pg_sage */ 2",
		"SELECT a$b$; SELECT 2": "SELECT /* pg_sage */ a$b$; SELECT /* pg_sage */ 2",
		"SELECT $1; SELECT 2":   "SELECT /* pg_sage */ $1; SELECT /* pg_sage */ 2",
		"SELECT $1$x$;$x$; SELECT 2": "SELECT /* pg_sage */ $1$x$;$x$; SELECT " +
			"/* pg_sage */ 2",
		"SELECT 1 -- a;b\n; SELECT 2": "SELECT /* pg_sage */ 1 -- a;b\n; SELECT " +
			"/* pg_sage */ 2",
		"SELECT 1 /* a; /* b; */ c; */; SELECT 2": "SELECT /* pg_sage */ 1 /* a; /* b; */ " +
			"c; */; SELECT /* pg_sage */ 2",
		"CREATE RULE r AS ON INSERT TO t DO INSTEAD (INSERT INTO u VALUES (1); DELETE " +
			"FROM v); SELECT 2": "CREATE /* pg_sage */ RULE r AS ON INSERT TO t DO INSTEAD " +
			"(INSERT INTO u VALUES (1); DELETE FROM v); SELECT /* pg_sage */ 2",
		"SELECT 1;":             "SELECT /* pg_sage */ 1;",
		"SELECT 1;;  ":          "SELECT /* pg_sage */ 1;;  ",
		"SELECT 1; -- end":      "SELECT /* pg_sage */ 1; -- end",
		"SELECT 1;(SELECT 2)":   "SELECT /* pg_sage */ 1;( /* pg_sage */ SELECT 2)",
		"SELECT ä$x$; SELECT 2": "SELECT /* pg_sage */ ä$x$; SELECT /* pg_sage */ 2",
	}
	for in, want := range cases {
		if got := queryThroughTagger(t, in); got != want {
			t.Errorf("%q\n  got  %q\n  want %q", in, got, want)
		}
	}
}

// When the statement boundaries cannot be read with certainty, only the
// first statement is tagged (its leading text is read the same way in any
// case): a backslash in a quoted literal depends on the server's
// standard_conforming_strings, and an unterminated quote or comment has
// no end to find. A tag is never risked inside a literal.
func TestTagConn_UncertainBoundariesTagOnlyTheFirstStatement(t *testing.T) {
	cases := map[string]string{
		`SELECT E'a\'; b'; SELECT 2`: `SELECT /* pg_sage */ E'a\'; b'; SELECT 2`,
		`SELECT 'C:\'; SELECT 'x; DELETE'`: `SELECT /* pg_sage */ 'C:\'; ` +
			`SELECT 'x; DELETE'`,
		"SELECT 'unterminated; SELECT 2":   "SELECT /* pg_sage */ 'unterminated; SELECT 2",
		`SELECT "unterminated; SELECT 2`:   `SELECT /* pg_sage */ "unterminated; SELECT 2`,
		"SELECT $q$ open; SELECT 2":        "SELECT /* pg_sage */ $q$ open; SELECT 2",
		"SELECT 1 /* open; SELECT 2":       "SELECT /* pg_sage */ 1 /* open; SELECT 2",
		`SELECT U&'d\0061t'; SELECT 2`:     `SELECT /* pg_sage */ U&'d\0061t'; SELECT 2`,
		"SELECT 1; SELECT 'open; SELECT 3": "SELECT /* pg_sage */ 1; SELECT 'open; SELECT 3",
	}
	for in, want := range cases {
		if got := queryThroughTagger(t, in); got != want {
			t.Errorf("%q\n  got  %q\n  want %q", in, got, want)
		}
	}
}

// A Parse message holds one statement (the server rejects more), so only
// its first statement is tagged, as before.
func TestTagConn_ParseTagsOneStatement(t *testing.T) {
	cc := &captureConn{}
	writeAll(t, TagConn(cc), append(startup(t), encode(t,
		&pgproto3.Parse{Query: "SELECT $1; SELECT 2"})...), 1<<20)
	p, ok := decodeFrontend(t, cc.out.Bytes())[0].(*pgproto3.Parse)
	if !ok || p.Query != "SELECT /* pg_sage */ $1; SELECT 2" {
		t.Fatalf("parse = %#v, want only the first statement tagged", p)
	}
}

// stripTagsAndSpace removes every pg_sage tag and all white space, which
// is everything the tagger may add or move.
func stripTagsAndSpace(s string) string {
	s = strings.ReplaceAll(s, StatementTag, "")
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// FuzzPlaceTags: tagging never panics, adds nothing but tags and white
// space to text without a pg_sage comment (one that exists may be moved,
// which this comparison cannot follow), and is idempotent (a tagged
// statement is not tagged again).
func FuzzPlaceTags(f *testing.F) {
	for _, seed := range []string{
		"SELECT 1; SELECT 2", "DO $$ BEGIN PERFORM 1; END $$; SELECT 'a;b'",
		"/* pg_sage */ (SELECT 1); /* x */ SELECT \"q;\"", "SELECT a$b$; SELECT $1",
		"SELECT E'\\'; x'", "-- c\n;;", "/* /* */", "$a$$a$;$$", "",
		"A$$ $$;0", // found by fuzzing: the tag split the identifier A$$
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		once := placeTags(sql)
		if twice := placeTags(once); twice != once {
			t.Fatalf("not idempotent:\n once  %q\n twice %q", once, twice)
		}
		if strings.Contains(sql, ApplicationName) {
			return
		}
		if stripTagsAndSpace(once) != stripTagsAndSpace(sql) {
			t.Fatalf("tagging changed more than tags and space:\n in  %q\n out %q", sql, once)
		}
	})
}
