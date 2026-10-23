//go:build cgo

package sqlast

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

type corpusEntry struct {
	SQL  string         `json:"sql"`
	Want []DDLStatement `json:"want"`
}

func loadDDLCorpus(t *testing.T) []corpusEntry {
	t.Helper()
	raw, err := os.ReadFile("testdata/ddl_corpus.json")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var corpus []corpusEntry
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	return corpus
}

// buildCorpusScript joins the corpus into one migration script. Every third
// statement carries a leading block comment and every fourth a line comment,
// so offsets must skip both. It returns the script and, per entry, the byte
// offset at which its SQL was placed.
func buildCorpusScript(corpus []corpusEntry) (string, []int) {
	var b strings.Builder
	b.WriteString("-- change review corpus\n")
	offsets := make([]int, len(corpus))
	for i, entry := range corpus {
		switch {
		case i%3 == 0:
			b.WriteString("/* step */ ")
		case i%4 == 0:
			b.WriteString("-- next\n  ")
		}
		offsets[i] = b.Len()
		b.WriteString(entry.SQL)
		if i%2 == 0 {
			b.WriteString("  ")
		}
		b.WriteString(";\n")
	}
	return b.String(), offsets
}

// CHECK-SQL-01: every corpus statement comes back with the expected kind,
// relation, columns, index, constraint and column fields, flags, timeouts,
// transaction state and byte offsets.
func TestInspectDDLCorpus(t *testing.T) {
	corpus := loadDDLCorpus(t)
	if len(corpus) < 45 {
		t.Fatalf("corpus has %d statements, want at least 45", len(corpus))
	}
	script, offsets := buildCorpusScript(corpus)
	var want []DDLStatement
	for i, entry := range corpus {
		line := 1 + strings.Count(script[:offsets[i]], "\n")
		for _, w := range entry.Want {
			w.Statement, w.Offset, w.Line, w.Text = i, offsets[i], line, entry.SQL
			want = append(want, w)
		}
	}
	got, err := InspectDDL(script)
	if err != nil {
		t.Fatalf("InspectDDL(corpus) error = %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("InspectDDL returned %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			g, _ := json.Marshal(got[i])
			w, _ := json.Marshal(want[i])
			t.Errorf("entry %d (%q):\n got  %s\n want %s", i, want[i].Text, g, w)
		}
	}
}

// Each corpus statement inspected alone gives the same typed result as in
// the script (apart from position and script state), so callers may inspect
// statement by statement.
func TestInspectDDLSingleStatementsMatchScript(t *testing.T) {
	for _, entry := range loadDDLCorpus(t) {
		got, err := InspectDDL(entry.SQL)
		if err != nil {
			t.Errorf("InspectDDL(%q) error = %v", entry.SQL, err)
			continue
		}
		if len(got) != len(entry.Want) {
			t.Errorf("%q: %d entries, want %d", entry.SQL, len(got), len(entry.Want))
			continue
		}
		for i, g := range got {
			w := entry.Want[i]
			if g.Offset != 0 || g.Line != 1 || g.Text != entry.SQL || g.Statement != 0 {
				t.Errorf("%q: position = %d/%d/%q", entry.SQL, g.Offset, g.Line, g.Text)
			}
			if g.Kind != w.Kind || g.Command != w.Command || g.Relation != w.Relation ||
				!reflect.DeepEqual(g.Columns, w.Columns) || g.Part != w.Part {
				t.Errorf("%q part %d: got %+v, want %+v", entry.SQL, i, g, w)
			}
		}
	}
}

func TestInspectDDLOffsetsSkipCommentsAndWhitespace(t *testing.T) {
	script := "\n\n  -- header\n/* multi\nline */  CREATE INDEX i ON t (a) ;" +
		"\t-- trailing\n\nDROP TABLE t2"
	got, err := InspectDDL(script)
	if err != nil {
		t.Fatalf("InspectDDL error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2", len(got))
	}
	first := strings.Index(script, "CREATE")
	if got[0].Offset != first || got[0].Line != 5 || got[0].Text != "CREATE INDEX i ON t (a)" {
		t.Errorf("first = %d/%d/%q, want %d/5/%q",
			got[0].Offset, got[0].Line, got[0].Text, first, "CREATE INDEX i ON t (a)")
	}
	second := strings.Index(script, "DROP")
	if got[1].Offset != second || got[1].Line != 7 || got[1].Text != "DROP TABLE t2" ||
		got[1].Statement != 1 {
		t.Errorf("second = %d/%d/%q/%d, want %d/7/DROP TABLE t2/1",
			got[1].Offset, got[1].Line, got[1].Text, got[1].Statement, second)
	}
	for _, s := range got {
		if script[s.Offset:s.Offset+len(s.Text)] != s.Text {
			t.Errorf("Text %q is not script[%d:]", s.Text, s.Offset)
		}
	}
}

func TestInspectDDLMultibyteOffsets(t *testing.T) {
	script := "COMMENT ON TABLE t IS 'café ☕';\nALTER TABLE t ADD COLUMN \"naïve\" text"
	got, err := InspectDDL(script)
	if err != nil {
		t.Fatalf("InspectDDL error = %v", err)
	}
	want := strings.Index(script, "ALTER")
	if len(got) != 2 || got[1].Offset != want || got[1].Columns[0] != "naïve" {
		t.Fatalf("got %+v, want ALTER at byte %d naming naïve", got, want)
	}
}

// CHECK-SQL-02: statements between BEGIN and COMMIT are in a block;
// ROLLBACK, COMMIT AND CHAIN, START TRANSACTION and END are tracked too.
func TestInspectDDLTransactionBlocks(t *testing.T) {
	script := `CREATE INDEX CONCURRENTLY a ON t (x);
BEGIN;
ALTER TABLE t ADD COLUMN y int;
CREATE INDEX CONCURRENTLY b ON t (y);
COMMIT;
CREATE INDEX c ON t (z);
START TRANSACTION;
DROP INDEX d;
COMMIT AND CHAIN;
DROP INDEX e;
ROLLBACK;
DROP INDEX f;
BEGIN;
SAVEPOINT s1;
DROP INDEX g;
ROLLBACK TO SAVEPOINT s1;
END;
DROP INDEX h`
	got, err := InspectDDL(script)
	if err != nil {
		t.Fatalf("InspectDDL error = %v", err)
	}
	want := []struct {
		inBlock bool
		txn     string
	}{
		{false, ""}, {false, "begin"}, {true, ""}, {true, ""}, {true, "commit"},
		{false, ""}, {false, "start"}, {true, ""}, {true, "commit"}, {true, ""},
		{true, "rollback"}, {false, ""}, {false, "begin"}, {true, "savepoint"},
		{true, ""}, {true, "rollback_to"}, {true, "commit"}, {false, ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d statements, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].InTransactionBlock != w.inBlock || got[i].Transaction != w.txn {
			t.Errorf("statement %d %q: in block %v txn %q, want %v %q", i, got[i].Text,
				got[i].InTransactionBlock, got[i].Transaction, w.inBlock, w.txn)
		}
		if w.txn != "" && got[i].Kind != DDLTransaction {
			t.Errorf("statement %d kind = %s, want transaction", i, got[i].Kind)
		}
	}
}

func ms(v int64) *int64 { return &v }

func timeoutsOf(t *testing.T, script string) []*int64 {
	t.Helper()
	got, err := InspectDDL(script)
	if err != nil {
		t.Fatalf("InspectDDL(%q) error = %v", script, err)
	}
	out := make([]*int64, len(got))
	for i, s := range got {
		out[i] = s.LockTimeoutMs
	}
	return out
}

func TestInspectDDLLockTimeoutTracking(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   []*int64
	}{
		{"units", "SET lock_timeout = '1.5s'; SELECT 1; SET lock_timeout = 250; SELECT 1;" +
			" SET lock_timeout = '2h'; SELECT 1; SET lock_timeout = '1d'; SELECT 1;" +
			" SET lock_timeout = '500ms'; SELECT 1; SET lock_timeout = '3000us'; SELECT 1",
			[]*int64{nil, ms(1500), ms(1500), ms(250), ms(250), ms(7200000), ms(7200000),
				ms(86400000), ms(86400000), ms(500), ms(500), ms(3)}},
		{"zero disables", "SET lock_timeout = 0; SELECT 1", []*int64{nil, ms(0)}},
		{"default resets", "SET lock_timeout = '5s'; SET lock_timeout TO DEFAULT; SELECT 1",
			[]*int64{nil, ms(5000), nil}},
		{"reset all", "SET lock_timeout = '5s'; SET statement_timeout = 9; RESET ALL; SELECT 1",
			[]*int64{nil, ms(5000), ms(5000), nil}},
		{"bad value keeps previous", "SET lock_timeout = '5s'; SET lock_timeout = 'soon';" +
			" SELECT 1", []*int64{nil, ms(5000), ms(5000)}},
		{"local outside a block has no effect", "SET LOCAL lock_timeout = '5s'; SELECT 1",
			[]*int64{nil, nil}},
		{"rollback undoes a session SET", "BEGIN; SET lock_timeout = '5s'; ROLLBACK; SELECT 1",
			[]*int64{nil, nil, ms(5000), nil}},
		{"commit keeps a session SET", "BEGIN; SET lock_timeout = '5s'; COMMIT; SELECT 1",
			[]*int64{nil, nil, ms(5000), ms(5000)}},
		{"session SET under a local one", "BEGIN; SET LOCAL lock_timeout = '1s';" +
			" SET lock_timeout = '9s'; COMMIT; SELECT 1",
			[]*int64{nil, nil, ms(1000), ms(9000), ms(9000)}},
		{"quoted name and SESSION", `SET SESSION "lock_timeout" = '4s'; SELECT 1`,
			[]*int64{nil, ms(4000)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := timeoutsOf(t, c.script)
			if len(got) != len(c.want) {
				t.Fatalf("got %d statements, want %d", len(got), len(c.want))
			}
			for i := range c.want {
				if !reflect.DeepEqual(got[i], c.want[i]) {
					t.Errorf("statement %d lock timeout = %v, want %v",
						i, deref(got[i]), deref(c.want[i]))
				}
			}
		})
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestInspectDDLStatementTimeoutIndependentOfLockTimeout(t *testing.T) {
	got, err := InspectDDL("SET statement_timeout = '30s'; SET lock_timeout = '1s';" +
		" RESET statement_timeout; SELECT 1")
	if err != nil {
		t.Fatalf("InspectDDL error = %v", err)
	}
	last := got[len(got)-1]
	if last.StatementTimeoutMs != nil || !reflect.DeepEqual(last.LockTimeoutMs, ms(1000)) {
		t.Errorf("last = stmt %v lock %v, want nil and 1000",
			deref(last.StatementTimeoutMs), deref(last.LockTimeoutMs))
	}
	if !reflect.DeepEqual(got[2].StatementTimeoutMs, ms(30000)) {
		t.Errorf("RESET sees statement timeout %v, want 30000", deref(got[2].StatementTimeoutMs))
	}
}

// CHECK-SQL-02: a parse error anywhere rejects the whole script with the
// parser's message and the failing byte offset; nothing partial comes back.
func TestInspectDDLParseErrorRejectsWholeScript(t *testing.T) {
	cases := []struct {
		script, token, message string
		line                   int
	}{
		{"CREATE INDEX i ON t (a);\nALTER TABLE t ADD COLUMN;", ";", "syntax error", 2},
		{"SELECT 'é';\nSELEC 1", "SELEC", `syntax error at or near "SELEC"`, 2},
		{"ALTER TABLE t ADD COLUMN x int;\nDROP TABLE;\nCREATE INDEX j ON t (b)",
			";\nCREATE", "syntax error", 2},
	}
	for _, c := range cases {
		got, err := InspectDDL(c.script)
		if got != nil {
			t.Errorf("%q: got partial result %+v", c.script, got)
		}
		if !errors.Is(err, ErrRejected) {
			t.Fatalf("%q: error = %v, want ErrRejected", c.script, err)
		}
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("%q: error %T is not a *ParseError", c.script, err)
		}
		wantOffset := strings.LastIndex(c.script, c.token)
		if !strings.Contains(pe.Message, c.message) || pe.Offset != wantOffset ||
			pe.Line != c.line {
			t.Errorf("%q: ParseError = %+v, want %q at %d line %d",
				c.script, pe, c.message, wantOffset, c.line)
		}
		if !strings.Contains(err.Error(), pe.Message) {
			t.Errorf("error text %q lacks the parser message", err.Error())
		}
	}
}

// CHECK-SQL-07: PostgreSQL 18-only syntax fails the PG17 parser pg_query_go
// embeds; the script is rejected, never half-inspected.
func TestInspectDDLRejectsPG18OnlySyntax(t *testing.T) {
	for _, sql := range []string{
		"CREATE TABLE rv.v (a int, b int GENERATED ALWAYS AS (a * 2) VIRTUAL)",
		"ALTER TABLE rv.orders ADD CONSTRAINT c CHECK (amount > 0) NOT ENFORCED",
	} {
		script := "ALTER TABLE rv.orders ADD COLUMN note text;\n" + sql
		got, err := InspectDDL(script)
		var pe *ParseError
		if got != nil || !errors.Is(err, ErrRejected) || !errors.As(err, &pe) {
			t.Errorf("%q: got %v, %v; want nil, *ParseError", sql, got, err)
			continue
		}
		if pe.Line != 2 || pe.Offset <= len("ALTER TABLE rv.orders ADD COLUMN note text;") {
			t.Errorf("%q: error at %d line %d, want inside line 2", sql, pe.Offset, pe.Line)
		}
	}
}

func TestInspectDDLEmptyInputs(t *testing.T) {
	for _, sql := range []string{"", "   \n\t", "-- only a comment", "/* c */ ;  ;"} {
		got, err := InspectDDL(sql)
		if err != nil || len(got) != 0 {
			t.Errorf("InspectDDL(%q) = %v, %v; want no statements, nil", sql, got, err)
		}
	}
}

func TestInspectDDLOtherStatements(t *testing.T) {
	cases := map[string]string{
		"DO $$ BEGIN PERFORM 1; END $$":                                "DoStmt",
		"VACUUM rv.orders":                                             "VacuumStmt",
		"COMMENT ON TABLE rv.orders IS 'x'":                            "CommentStmt",
		"ALTER INDEX rv.i RENAME TO j":                                 "RenameStmt",
		"MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN DELETE": "MergeStmt",
		"CREATE VIEW rv.v AS SELECT 1":                                 "ViewStmt",
		"DROP SEQUENCE rv.s":                                           "DropStmt",
	}
	for sql, command := range cases {
		got, err := InspectDDL(sql)
		if err != nil || len(got) != 1 {
			t.Fatalf("InspectDDL(%q) = %v, %v", sql, got, err)
		}
		if got[0].Kind != DDLOther || got[0].Command != command {
			t.Errorf("%q: kind %s command %s, want other %s",
				sql, got[0].Kind, got[0].Command, command)
		}
	}
}
