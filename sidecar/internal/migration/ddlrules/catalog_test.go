package ddlrules

import "testing"

// pgLockModes are PostgreSQL's table lock modes; "" is allowed only for
// the lock_timeout annotation, which takes no lock of its own.
var pgLockModes = map[string]bool{
	"ACCESS SHARE": true, "ROW SHARE": true, "ROW EXCLUSIVE": true,
	"SHARE UPDATE EXCLUSIVE": true, "SHARE": true, "SHARE ROW EXCLUSIVE": true,
	"EXCLUSIVE": true, "ACCESS EXCLUSIVE": true,
}

// The catalog is the one lock table the classifier and the case preflight
// share (migration-01): every rule has a unique id, a real lock mode and a
// safer alternative.
func TestCatalog_RulesAreWellFormed(t *testing.T) {
	rules := Catalog()
	if len(rules) < 20 {
		t.Fatalf("catalog has %d rules, want the full catalog", len(rules))
	}
	seen := map[string]bool{}
	for _, r := range rules {
		if r.ID == "" || seen[r.ID] {
			t.Errorf("rule id %q is empty or duplicated", r.ID)
		}
		seen[r.ID] = true
		if !pgLockModes[r.LockLevel] && r.ID != "ddl_missing_lock_timeout" {
			t.Errorf("%s lock level %q is not a PostgreSQL lock mode", r.ID, r.LockLevel)
		}
		if r.Description == "" || r.SafeAltTemplate == "" {
			t.Errorf("%s has no description or safe alternative", r.ID)
		}
	}
}

// Boundary values the preflight relies on: the plan's corrected levels
// and the catalog's rewrite flags.
func TestLookup_LockLevelAndRewrite(t *testing.T) {
	tests := []struct {
		id      string
		lock    string
		rewrite bool
	}{
		{"ddl_constraint_not_valid", "ACCESS EXCLUSIVE", false},
		{"ddl_fk_not_valid", "SHARE ROW EXCLUSIVE", false},
		{"ddl_vacuum_full", "ACCESS EXCLUSIVE", true},
		{"ddl_cluster", "ACCESS EXCLUSIVE", true},
		{"ddl_add_column_volatile_default", "ACCESS EXCLUSIVE", true},
		{"ddl_index_not_concurrent", "SHARE", false},
	}
	for _, tc := range tests {
		r, ok := Lookup(tc.id)
		if !ok || r.ID != tc.id || r.LockLevel != tc.lock || r.RequiresRewrite != tc.rewrite {
			t.Errorf("Lookup(%s) = %+v, %v; want lock %s rewrite %v",
				tc.id, r, ok, tc.lock, tc.rewrite)
		}
	}
}

// Invalid and empty ids are not rules.
func TestLookup_UnknownID(t *testing.T) {
	for _, id := range []string{"", "ddl_unknown", "DDL_VACUUM_FULL", " ddl_cluster"} {
		if r, ok := Lookup(id); ok || r != (Rule{}) {
			t.Errorf("Lookup(%q) = %+v, %v; want zero rule, false", id, r, ok)
		}
	}
}

// No concurrent access test: the catalog is rebuilt on every call and
// shares no state.
