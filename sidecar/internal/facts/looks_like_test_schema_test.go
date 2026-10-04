package facts

import "testing"

// LooksLikeTestSchema is the fixture detector's catalog regex, applied in
// Go: it must agree with the ~* match the detector runs.
func TestLooksLikeTestSchema(t *testing.T) {
	for schema, want := range map[string]bool{
		"test_memory_ab12cd": true, "TEST_x": true, "pytest-run": true, "ci_42": true,
		"orders_test": true, "app_tmp": true, "a_test_b": true, "tmp-1": true,
		"public": false, "testing": false, "contest": false, "latest_orders": false,
		"attest_x": false, "": false, "tenant_42": false, "citus": false,
	} {
		if got := LooksLikeTestSchema(schema); got != want {
			t.Errorf("LooksLikeTestSchema(%q) = %t, want %t", schema, got, want)
		}
	}
}
