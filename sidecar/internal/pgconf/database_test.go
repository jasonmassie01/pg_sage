package pgconf

import "testing"

func TestParseAlterDatabasePreservesIdentifierAndInverseInputs(t *testing.T) {
	for _, tc := range []struct {
		sql, database, name, value string
		reset                      bool
	}{
		{`ALTER DATABASE "Mi x" SET work_mem='64MB'`, `"Mi x"`, "work_mem", "64MB", false},
		{"ALTER\nDATABASE app\nRESET work_mem;", "app", "work_mem", "", true},
		{`ALTER DATABASE "a""b" SET random_page_cost TO 1.25`, `"a""b"`, "random_page_cost", "1.25", false},
	} {
		got, ok := ParseAlterDatabase(tc.sql)
		if !ok || got.Database != tc.database || got.Name != tc.name || got.Value != tc.value || got.Reset != tc.reset {
			t.Fatalf("parse %q: %+v ok=%v", tc.sql, got, ok)
		}
	}
	for _, sql := range []string{"ALTER DATABASE app RESET ALL", "ALTER DATABASE app RENAME TO other", "ALTER SYSTEM SET work_mem='64MB'"} {
		if _, ok := ParseAlterDatabase(sql); ok {
			t.Fatalf("unexpected database setting parse: %s", sql)
		}
	}
}
