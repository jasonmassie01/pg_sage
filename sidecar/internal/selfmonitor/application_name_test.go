package selfmonitor

import "testing"

// IsApplicationName is ActivityExclusionSQL's Go twin, for sources that
// carry application_name outside SQL (log records, auto_explain plans).
func TestIsApplicationName(t *testing.T) {
	for name, want := range map[string]bool{
		"pg_sage":             true,
		"PG_SAGE":             true,
		"pg_sage-fleet-b":     true,
		"  pg_sage  ":         true,
		"selfload_app":        false,
		"psql":                false,
		"":                    false,
		"pgsage":              false, // not pg_sage's name
		"pg_sag":              false,
		"app talking pg_sage": true, // the SQL predicate matches anywhere too
		"pg-sage":             true, // ILIKE's _ is any one character
	} {
		if got := IsApplicationName(name); got != want {
			t.Errorf("IsApplicationName(%q) = %v, want %v", name, got, want)
		}
	}
}

// The Go and SQL predicates agree on every name.
func TestIsApplicationNameMatchesActivityExclusionSQL(t *testing.T) {
	pool, ctx := selfPool(t)
	for _, name := range []string{"pg_sage", "PG_SAGE", "x pg_sage y", "app", "", "pgsage",
		"pg-sage"} {
		var excluded bool
		if err := pool.QueryRow(ctx, "SELECT NOT ("+
			ActivityExclusionSQL("a")+") FROM (SELECT $1::text AS application_name) a",
			name).Scan(&excluded); err != nil {
			t.Fatalf("predicate for %q: %v", name, err)
		}
		if excluded != IsApplicationName(name) {
			t.Errorf("%q: SQL excludes=%v, Go IsApplicationName=%v", name, excluded,
				IsApplicationName(name))
		}
	}
}
