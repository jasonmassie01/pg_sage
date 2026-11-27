package migration

import (
	"strings"
	"testing"
)

// No concurrent access tests: the rewrites are pure functions of the text.

// A guarded rewrite must be safe for every subcommand of the statement.
// An ALTER TABLE with several subcommands, or a DROP INDEX of several
// indexes or with CASCADE, gets no rule rewrite: rewriting only the last
// subcommand (or adding CONCURRENTLY where PostgreSQL refuses it) would
// present a statement that still scans, or does not run, as the safe form.
func TestGuardedRewriteRefusesSeveralSubcommands(t *testing.T) {
	cases := []struct{ rule, sql string }{
		{"ddl_set_not_null",
			"ALTER TABLE app.t ALTER COLUMN a SET NOT NULL, ALTER COLUMN b SET NOT NULL"},
		{"ddl_set_not_null", "ALTER TABLE app.t ADD COLUMN x int, ALTER COLUMN b SET NOT NULL"},
		{"ddl_constraint_not_valid",
			"ALTER TABLE app.t ADD CONSTRAINT c1 CHECK (a > 0), ADD CONSTRAINT c2 CHECK (b > 0)"},
		{"ddl_fk_not_valid", "ALTER TABLE app.o ADD CONSTRAINT fk FOREIGN KEY (c) " +
			"REFERENCES app.c (id), ADD COLUMN y int"},
		{"ddl_add_column_volatile_default",
			"ALTER TABLE app.t ADD COLUMN x int, ADD COLUMN id uuid DEFAULT gen_random_uuid()"},
		{"ddl_drop_index_not_concurrent", "DROP INDEX app.i1, app.i2"},
		{"ddl_drop_index_not_concurrent", "DROP INDEX app.i CASCADE"},
	}
	for _, tc := range cases {
		if got := ruleRewrite(tc.rule, tc.sql); got != "" {
			t.Errorf("%s %q: rewrote to %q, want no rule rewrite", tc.rule, tc.sql, got)
		}
	}
}

// Commas inside parentheses or a quoted identifier are not subcommands:
// a composite foreign key and a quoted column name are still rewritten.
func TestGuardedRewriteKeepsNestedCommas(t *testing.T) {
	fk := "ALTER TABLE app.o ADD CONSTRAINT fk FOREIGN KEY (a, b) REFERENCES app.c (x, y)"
	want := fk + " NOT VALID;\nALTER TABLE app.o VALIDATE CONSTRAINT fk;"
	if got := ruleRewrite("ddl_fk_not_valid", fk); got != want {
		t.Fatalf("composite fk rewrite %q, want %q", got, want)
	}
	nn := `ALTER TABLE app.t ALTER COLUMN "a,b" SET NOT NULL`
	got := ruleRewrite("ddl_set_not_null", nn)
	if !strings.Contains(got, `CHECK ("a,b" IS NOT NULL) NOT VALID;`) ||
		!strings.Contains(got, `ALTER COLUMN "a,b" SET NOT NULL;`) {
		t.Fatalf("quoted column rewrite %q", got)
	}
	if got := ruleRewrite("ddl_drop_index_not_concurrent", `DROP INDEX app."i,j"`); got !=
		"DROP INDEX CONCURRENTLY app.\"i,j\";\n"+
			"-- CONCURRENTLY cannot run inside a transaction block." {
		t.Fatalf("quoted index rewrite %q", got)
	}
}

// Nil/empty: the subcommand scan of an empty or unbalanced text.
func TestHasTopLevelComma(t *testing.T) {
	cases := map[string]bool{
		"":                 false,
		"a, b":             true,
		"f(a, b)":          false,
		`"x,y"`:            false,
		"f(a, b), c":       true,
		"f((a), b)":        false,
		"close) , early":   true,
		`"unterminated, x`: false,
	}
	for in, want := range cases {
		if got := hasTopLevelComma(in); got != want {
			t.Errorf("hasTopLevelComma(%q) = %v, want %v", in, got, want)
		}
	}
}
