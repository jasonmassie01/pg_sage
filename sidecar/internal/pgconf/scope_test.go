package pgconf

import "testing"

// advisors-07: idle_in_transaction_session_timeout is documented (so the
// advisor may propose it as SQL) but only per database and only with an
// operator's approval. statement_timeout and idle_session_timeout stay
// undocumented, so they are advisory at every scope.

// No concurrent access tests: the scope rule reads package-level maps that
// are never written after init.

func TestTimeoutScope_DatabaseScopeApprovalOnly(t *testing.T) {
	cases := []struct {
		name                          string
		advisor, autonomous, dbScoped bool
	}{
		{"idle_in_transaction_session_timeout", true, false, true},
		{"IDLE_IN_TRANSACTION_SESSION_TIMEOUT", true, false, true},
		{" idle_in_transaction_session_timeout ", true, false, true},
		{"statement_timeout", false, false, false},
		{"idle_session_timeout", false, false, false},
		{"work_mem", true, true, false},
		{"max_connections", true, false, false},
		{"", false, false, false},
	}
	for _, c := range cases {
		if got := AdvisorGUC(c.name); got != c.advisor {
			t.Errorf("AdvisorGUC(%q) = %v, want %v", c.name, got, c.advisor)
		}
		if got := AutonomousGUC(c.name); got != c.autonomous {
			t.Errorf("AutonomousGUC(%q) = %v, want %v", c.name, got, c.autonomous)
		}
		if got := DatabaseScopeApprovalOnly(c.name); got != c.dbScoped {
			t.Errorf("DatabaseScopeApprovalOnly(%q) = %v, want %v", c.name, got, c.dbScoped)
		}
	}
	if !ExecutableGUC("idle_in_transaction_session_timeout") {
		t.Error("an approved database-scope timeout must pass the executor whitelist")
	}
	for _, name := range []string{"statement_timeout", "idle_session_timeout"} {
		if ExecutableGUC(name) {
			t.Errorf("ExecutableGUC(%q) = true; it must stay advisory-only", name)
		}
	}
}

// Every database-scope setting is documented and executable: a rule for a
// setting the advisor cannot propose would be dead.
func TestTimeoutScope_RuleCoversOnlyDocumentedSettings(t *testing.T) {
	for name := range databaseScopeApprovalOnly {
		if !AdvisorGUC(name) || !ExecutableGUC(name) || RequiresRestart(name) {
			t.Errorf("%s: database-scope rule needs a reloadable documented setting", name)
		}
	}
}

func TestTimeoutScope_SafeRangeBoundaries(t *testing.T) {
	const name = "idle_in_transaction_session_timeout"
	cases := []struct {
		value string
		ok    bool
	}{
		{"0", false},        // disabled is never a recommendation
		{"'0'", false},      // quoted the same
		{"0s", false},       // zero with a unit
		{"10s", false},      // below 60 s
		{"'10s'", false},    // quoted, too short
		{"59s", false},      // just below the floor
		{"59999", false},    // base unit ms, just below the floor
		{"60000", true},     // exactly 60 s in the base unit
		{"60s", true},       // the floor in seconds
		{"1min", true},      // the floor in minutes
		{"'5min'", true},    // a typical value
		{"24h", true},       // the ceiling
		{"86400000", true},  // the ceiling in base units
		{"86400001", false}, // just above 24 h in ms
		{"86401s", false},   // just above 24 h in seconds
		{"25h", false},      // above the ceiling
		{"-1", false},       // not a sentinel for this setting
		{"forever", false},  // not a value
		{"", false},         // empty
		{"2minutes", false}, // not a PostgreSQL unit
	}
	for _, c := range cases {
		if ok, reason := ValidateValue(name, c.value); ok != c.ok {
			t.Errorf("ValidateValue(%s, %q) = %v (%s), want %v", name, c.value, ok,
				reason, c.ok)
		}
	}
}
