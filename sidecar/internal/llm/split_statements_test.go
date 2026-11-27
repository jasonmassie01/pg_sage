package llm

import (
	"reflect"
	"strings"
	"testing"
)

// llm-08: the briefing grounder finds statements in model output with the
// same quote rules the redactor uses. Joining the pieces must give back the
// input, so a caller can rewrite one statement and keep the rest verbatim.
func TestSplitStatements(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"one without semicolon", "VACUUM orders", []string{"VACUUM orders"}},
		{"two statements", "VACUUM a;\nANALYZE b;\n",
			[]string{"VACUUM a;", "\nANALYZE b;", "\n"}},
		{"semicolon in literal", "SELECT ';' ; DROP TABLE x;",
			[]string{"SELECT ';' ;", " DROP TABLE x;"}},
		{"semicolon in E-string", `SELECT E'a\';b'; SELECT 2`,
			[]string{`SELECT E'a\';b';`, " SELECT 2"}},
		{"semicolon in identifier", `SELECT 1 AS "a;b"; SELECT 2`,
			[]string{`SELECT 1 AS "a;b";`, " SELECT 2"}},
		{"semicolon in dollar body", "DO $$BEGIN PERFORM 1; END$$; SELECT 2",
			[]string{"DO $$BEGIN PERFORM 1; END$$;", " SELECT 2"}},
		{"semicolon in line comment", "-- a; b\nSELECT 1;",
			[]string{"-- a; b\nSELECT 1;"}},
		{"semicolon in block comment", "/* a; /* b; */ c; */ SELECT 1;",
			[]string{"/* a; /* b; */ c; */ SELECT 1;"}},
		{"unterminated literal swallows the rest", "SELECT 'a; DROP TABLE x;",
			[]string{"SELECT 'a; DROP TABLE x;"}},
		{"stray double quote does not shield", `SELECT "a; DROP TABLE x;`,
			[]string{`SELECT "a;`, " DROP TABLE x;"}},
		{"only semicolons", ";;", []string{";", ";"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitStatements(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SplitStatements(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if joined := strings.Join(got, ""); joined != tc.in {
				t.Fatalf("pieces join to %q, want the input %q", joined, tc.in)
			}
		})
	}
}

// No concurrent access test: SplitStatements is a pure function of its input.
