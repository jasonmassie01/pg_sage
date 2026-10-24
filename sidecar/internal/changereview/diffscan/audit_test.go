package diffscan

import (
	"strings"
	"testing"
)

// Post-test audit additions (mutation survivors): documentation inside a
// migrations directory, context-only edits of the content-hash input, a
// format-patch trailer after the last hunk, and long tokens that are
// mixed case but repetitive.

func TestClassify_DocsInsideDataAccessDirsAreNotDataAccess(t *testing.T) {
	for _, p := range []string{"db/migrate/README.md", "migrations/README",
		"app/models/diagram.png", "prisma/migrations/notes.txt"} {
		f := Parse(buildDiff([]fixtureFile{{path: p, added: []string{"Order.where(x: 1)"}}}))
		if len(f) != 1 {
			t.Fatalf("%s: parsed %+v", p, f)
		}
		if c := Classify(f[0]); c.DataAccess {
			t.Errorf("%s classified %+v, want not data access", p, c)
		}
	}
}

func TestHunkFilter_IgnoresContextLines(t *testing.T) {
	a := buildDiff([]fixtureFile{{path: "db/migrate/1_x.rb", added: []string{"add_index :t, :c"}}})
	b := strings.Replace(a, " context\n", " other context\n", 1)
	if a == b || HunkFilter(a) != HunkFilter(b) {
		t.Fatal("a context-only difference changed the filtered hunks")
	}
}

func TestParse_FormatPatchTrailerIsNotAHunkLine(t *testing.T) {
	diff := "diff --git a/x.sql b/x.sql\n--- a/x.sql\n+++ b/x.sql\n" +
		"@@ -1 +1,2 @@\n a\n+b\n-- \n2.40.1\n"
	f := Parse(diff)
	if len(f) != 1 || len(f[0].Hunks[0].Lines) != 2 {
		t.Fatalf("the signature trailer became hunk lines: %+v", f)
	}
}

func TestRedact_KeepsRepetitiveMixedCaseTokens(t *testing.T) {
	in := "AAAAAAAAAAAAAAAAaaaaaaaaaaaaaaaa11111111"
	if got := Redact(in); got != in {
		t.Fatalf("Redact(%q) = %q, want it unchanged (low entropy)", in, got)
	}
}
