package approvalcard

import "testing"

// advisors-07: a database-scope change is a person's decision when the
// setting is off the autonomous list, as the instance-scope one is.
// No concurrent access tests: gucReasons is a pure function of the SQL.
func TestAssembleDatabaseScopeGUC(t *testing.T) {
	cases := []struct {
		sql     string
		wantWhy bool
	}{
		{`ALTER DATABASE "app" SET idle_in_transaction_session_timeout = '5min'`, true},
		{`ALTER DATABASE app RESET idle_in_transaction_session_timeout;`, true},
		{`ALTER DATABASE app SET work_mem = '64MB'`, false},
	}
	for _, tc := range cases {
		a := queued(6, "alter_database_guc", tc.sql, "", "moderate")
		c := Assemble(Inputs{Database: "app", Action: a, Now: now, Contract: contractOf(t, a)})
		if got := hasCode(c, "guc_not_allowlisted"); got != tc.wantWhy {
			t.Errorf("%s: guc_not_allowlisted = %v, want %v (why %v)", tc.sql, got,
				tc.wantWhy, reasonCodes(c))
		}
		if hasCode(c, "guc_restart") {
			t.Errorf("%s: a reload setting carries no restart reason: %v", tc.sql,
				reasonCodes(c))
		}
	}
	if got := gucReasons("ALTER DATABASE SET work_mem = '1MB'"); got != nil {
		t.Errorf("malformed ALTER DATABASE: reasons %v, want none", got)
	}
}
