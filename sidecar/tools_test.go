package main

import (
	"strings"
	"testing"
)

func TestReviewDDLSafety_DropTable(t *testing.T) {
	result := reviewDDLSafety("DROP TABLE users;")
	warnings := result["warnings"].([]string)
	if len(warnings) == 0 {
		t.Fatal("expected warnings")
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "DROP TABLE") {
			found = true
		}
	}
	if !found {
		t.Error("expected DROP TABLE warning")
	}
}

func TestReviewDDLSafety_DropColumn(t *testing.T) {
	result := reviewDDLSafety("ALTER TABLE users DROP COLUMN email;")
	warnings := result["warnings"].([]string)
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "DROP COLUMN") {
			found = true
		}
	}
	if !found {
		t.Error("expected DROP COLUMN warning")
	}
}

func TestReviewDDLSafety_CreateIndexWithoutConcurrently(t *testing.T) {
	result := reviewDDLSafety("CREATE INDEX idx_foo ON bar (col);")
	warnings := result["warnings"].([]string)
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "CONCURRENTLY") {
			found = true
		}
	}
	if !found {
		t.Error("expected CONCURRENTLY warning")
	}
}

func TestReviewDDLSafety_SafeDDL(t *testing.T) {
	result := reviewDDLSafety("CREATE INDEX CONCURRENTLY idx_foo ON bar (col);")
	warnings := result["warnings"].([]string)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "No obvious risks") {
		t.Errorf("expected no-risk message, got %v", warnings)
	}
}

func TestReviewDDLSafety_Truncate(t *testing.T) {
	result := reviewDDLSafety("TRUNCATE orders;")
	warnings := result["warnings"].([]string)
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "TRUNCATE") {
			found = true
		}
	}
	if !found {
		t.Error("expected TRUNCATE warning")
	}
}

func TestReviewDDLSafety_Rename(t *testing.T) {
	result := reviewDDLSafety("ALTER TABLE users RENAME TO customers;")
	warnings := result["warnings"].([]string)
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "RENAME") {
			found = true
		}
	}
	if !found {
		t.Error("expected RENAME warning")
	}
}

func TestReviewDDLSafety_MultipleWarnings(t *testing.T) {
	result := reviewDDLSafety("DROP TABLE users; TRUNCATE orders;")
	warnings := result["warnings"].([]string)
	if len(warnings) < 2 {
		t.Errorf("expected at least 2 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestReviewDDLSafety_ReturnsMode(t *testing.T) {
	result := reviewDDLSafety("SELECT 1;")
	mode, ok := result["mode"].(string)
	if !ok || mode != "sidecar-only" {
		t.Errorf("expected mode 'sidecar-only', got %v", result["mode"])
	}
}

func TestReviewDDLSafety_ReturnsDDL(t *testing.T) {
	ddl := "CREATE TABLE foo (id int);"
	result := reviewDDLSafety(ddl)
	if result["ddl"] != ddl {
		t.Errorf("expected ddl to be echoed back, got %v", result["ddl"])
	}
}

func TestSanitize_AllowsValid(t *testing.T) {
	cases := []struct{ in, want string }{
		{"public.users", "public.users"},
		{"my_table-1", "my_table-1"},
		{"foo123", "foo123"},
		{"UPPER_CASE", "UPPER_CASE"},
	}
	for _, tc := range cases {
		if got := sanitize(tc.in); got != tc.want {
			t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitize_StripsInjection(t *testing.T) {
	cases := []struct{ in, want string }{
		{"users; DROP TABLE--", "usersDROPTABLE--"},
		{"table$name", "tablename"},
		{"foo'bar", "foobar"},
		{"a b c", "abc"},
		{"tab\tnewline\n", "tabnewline"},
	}
	for _, tc := range cases {
		if got := sanitize(tc.in); got != tc.want {
			t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitize_EmptyString(t *testing.T) {
	if got := sanitize(""); got != "" {
		t.Errorf("sanitize(%q) = %q, want empty", "", got)
	}
}
