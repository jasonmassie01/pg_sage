package executor

import "testing"

// vacuum-06 review: the precheck reads the one relation a statement names,
// quote-aware, and refuses a statement naming several (the server would
// skip the unprivileged ones with only a WARNING).

// No concurrent access tests: maintenanceTable is a pure function of its
// argument.

func TestMaintenanceTable(t *testing.T) {
	cases := []struct {
		name, sql, want string
	}{
		{"plain", "VACUUM public.t", "public.t"},
		{"terminated", "VACUUM public.t;", "public.t"},
		{"quoted", `VACUUM "public"."Orders";`, `"public"."Orders"`},
		{"quoted with space", `VACUUM "public"."my table"`, `"public"."my table"`},
		{"quoted with comma", `VACUUM "public"."a,b"`, `"public"."a,b"`},
		{"doubled quote", `VACUUM "public"."a"" b"`, `"public"."a"" b"`},
		{"option list", "VACUUM (FREEZE, ANALYZE) public.t", "public.t"},
		{"option literal", "VACUUM (BUFFER_USAGE_LIMIT '256 kB') public.t", "public.t"},
		{"legacy options", "vacuum freeze verbose analyze public.t", "public.t"},
		{"column list", "VACUUM ANALYZE public.t (a, b)", "public.t"},
		{"column list no space", "VACUUM ANALYZE public.t(a, b)", "public.t"},
		{"only", "VACUUM ONLY public.parent", "public.parent"},
		{"whitespace", "  VACUUM\n\tpublic.t  ", "public.t"},
		{"alter", "ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.05)",
			"public.t"},
		{"alter if exists only",
			`ALTER TABLE IF EXISTS ONLY "s"."t" SET (autovacuum_enabled = off)`, `"s"."t"`},
		// Invalid or unverifiable: no single relation.
		{"two relations", "VACUUM public.a, public.b", ""},
		{"two relations no space", "VACUUM public.a,public.b", ""},
		{"second after column list", "VACUUM ANALYZE public.a (x), public.b", ""},
		{"options then two", "VACUUM (ANALYZE) public.a, public.b;", ""},
		{"database wide", "VACUUM", ""},
		{"database wide terminated", "VACUUM;", ""},
		{"options only", "VACUUM (FREEZE)", ""},
		{"alter no table", "ALTER TABLE", ""},
		{"empty", "", ""},
		{"other statement", "ANALYZE public.t", ""},
	}
	for _, tc := range cases {
		if got := maintenanceTable(tc.sql); got != tc.want {
			t.Errorf("%s: maintenanceTable(%q) = %q, want %q", tc.name, tc.sql, got, tc.want)
		}
	}
}
