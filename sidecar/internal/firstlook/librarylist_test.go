package firstlook

import "testing"

// libraryList renders one SQL literal per library. A single literal such
// as 'a,b' names one library called "a,b": the server then refuses every
// new session (session_preload_libraries) or fails to start
// (shared_preload_libraries).
func TestLibraryListRendersOneLiteralPerLibrary(t *testing.T) {
	cases := []struct {
		name string
		list *string
		want string
	}{
		{"empty list", strp(""), "'auto_explain'"},
		{"blank list", strp("  "), "'auto_explain'"},
		{"one library kept", strp("pg_hint_plan"), "'pg_hint_plan', 'auto_explain'"},
		{"server spacing", strp("pg_stat_statements, pg_hint_plan"),
			"'pg_stat_statements', 'pg_hint_plan', 'auto_explain'"},
		{"already listed keeps its order", strp("auto_explain, pg_hint_plan"),
			"'auto_explain', 'pg_hint_plan'"},
		{"stray commas", strp(",pg_hint_plan,,"), "'pg_hint_plan', 'auto_explain'"},
		{"quoted element unquoted", strp(`"$libdir/plugins/My Lib", x`),
			`'$libdir/plugins/My Lib', 'x', 'auto_explain'`},
		{"comma inside quotes", strp(`"a,b", c`), "'a,b', 'c', 'auto_explain'"},
		{"doubled quote", strp(`"say ""hi"""`), `'say "hi"', 'auto_explain'`},
		{"single quote escaped", strp("o'brien"), "'o''brien', 'auto_explain'"},
		{"unreadable keeps placeholder", nil, "'<existing libraries>', 'auto_explain'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := libraryList(c.list, "auto_explain"); got != c.want {
				t.Fatalf("libraryList(%v) = %q, want %q", c.list, got, c.want)
			}
		})
	}
}

// No concurrent access test: libraryList is a pure function of its inputs.
