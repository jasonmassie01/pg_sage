package facts

import "testing"

// IsTestSchemaName is the fixture detector's catalog regex, applied in
// Go: it must agree with the ~* match the detector runs, including mixed
// case and "-" separators.
func TestIsTestSchemaNameMatchesTheCatalogRule(t *testing.T) {
	for schema, want := range map[string]bool{
		"test_memory_ab12cd": true, "TEST_x": true, "pytest-run": true, "ci_42": true,
		"orders_test": true, "app_tmp": true, "a_test_b": true, "tmp-1": true,
		"Tmp-Run": true, "Orders-TEST": true, "x-Test-y": true,
		"public": false, "testing": false, "contest": false, "latest_orders": false,
		"attest_x": false, "": false, "tenant_42": false, "citus": false,
	} {
		if got := IsTestSchemaName(schema); got != want {
			t.Errorf("IsTestSchemaName(%q) = %t, want %t", schema, got, want)
		}
	}
}

// The SQL the first look runs uses the same pattern text, unchanged.
func TestTestSchemaPatternIsTheCatalogRegex(t *testing.T) {
	want := `^(test|tests|testing|tmp|temp|pytest|ci)[_-]|` +
		`[_-](test|tests|tmp|temp)$|[_-]test[_-]`
	if got := TestSchemaPattern(); got != want {
		t.Fatalf("TestSchemaPattern() = %q, want %q", got, want)
	}
}
