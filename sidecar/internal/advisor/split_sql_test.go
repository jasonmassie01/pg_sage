package advisor

import "testing"

func TestSplitSQLStatements(t *testing.T) {
	multi := splitSQLStatements(
		"ALTER SYSTEM SET checkpoint_timeout = '900s'; ALTER SYSTEM SET max_wal_size = '16GB'; ALTER SYSTEM SET min_wal_size = '4GB';")
	if len(multi) != 3 {
		t.Fatalf("expected 3 statements, got %d: %v", len(multi), multi)
	}
	for _, s := range multi {
		if s[len(s)-1] != ';' {
			t.Errorf("statement should end with ;: %q", s)
		}
	}
	one := splitSQLStatements("ALTER SYSTEM SET work_mem = '16MB';")
	if len(one) != 1 {
		t.Errorf("single statement: got %d", len(one))
	}
	if len(splitSQLStatements("")) != 0 {
		t.Error("empty SQL should yield 0 statements")
	}
}

func TestParseLLMFindings_SplitsMultiStatement(t *testing.T) {
	raw := `[{"object_identifier":"instance","severity":"warning","rationale":"tune WAL",` +
		`"recommended_sql":"ALTER SYSTEM SET checkpoint_timeout = '900s'; ALTER SYSTEM SET max_wal_size = '16GB';"}]`
	fs := parseLLMFindings(raw, "wal_tuning", func(string, string, ...any) {})
	if len(fs) != 2 {
		t.Fatalf("expected 2 split findings, got %d", len(fs))
	}
	for _, f := range fs {
		if c := f.RecommendedSQL; len(c) == 0 || c[len(c)-1] != ';' {
			t.Errorf("each finding should be a single statement: %q", f.RecommendedSQL)
		}
		if f.ObjectIdentifier == "instance" {
			t.Error("split findings need distinct object identifiers")
		}
	}
}
