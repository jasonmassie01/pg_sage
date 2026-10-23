//go:build cgo

package impact

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// CR-18: SQL the PG17 parser rejects (here a typo, and PostgreSQL 18-only
// syntax) is analyzed in text mode with the parser's message, never safe.
func TestUnparsedFileFallsBackToTextMode(t *testing.T) {
	e := newEnv(t)
	in := input("")
	in.SQLFiles = []changereview.SQLFile{
		{Path: "001_ok.sql", Content: "ALTER TABLE rv.rv_orders ADD COLUMN note text"},
		{Path: "002_typo.sql", Content: "ALTER TABLE rv.rv_orders ADD COLUM x int"},
	}
	a := e.analyzeWith(e.eng, in)
	if a.ParseMode != changereview.ParseText {
		t.Fatalf("parse mode %s, want text when any file is unparsed", a.ParseMode)
	}
	f := one(t, a, changereview.RuleTextMode)
	if f.Severity != changereview.SeverityWarning || !strings.Contains(f.Text, "002_typo.sql") ||
		!strings.Contains(f.Text, "syntax error") {
		t.Fatalf("CR-18 %+v, want the file and the parser's message", f)
	}
	if a.Statements[0].Kind != "add_column" {
		t.Fatalf("the parsed file keeps its AST statements: %+v", a.Statements[0])
	}
	if verdict(a) == changereview.VerdictSafe {
		t.Fatal("an unparsed file must never be safe")
	}
	pg18 := e.analyze("ALTER TABLE rv.rv_orders ADD COLUMN v int GENERATED ALWAYS AS " +
		"(customer_id * 2) VIRTUAL")
	if pg18.ParseMode != changereview.ParseText || verdict(pg18) == changereview.VerdictSafe {
		t.Fatalf("PG18-only syntax: mode %s verdict %s, want text and not safe",
			pg18.ParseMode, verdict(pg18))
	}
}
