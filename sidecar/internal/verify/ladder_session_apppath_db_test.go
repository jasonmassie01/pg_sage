package verify

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-n: a session-setting step's targets are the application's
// statements, so they are planned on the application's search_path even
// when the session is pinned to pg_catalog, pg_temp, and a decoy table in
// another schema never answers for them.
// No concurrent access tests: the path is transaction-local (set_config
// is_local) on the caller's own session.

func TestFromSessionSetting_PlansOnTheApplicationPathUnderThePin(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "verify_ladder_apppath")
	r := FromSessionSetting(t.Context(), pinned, SessionSetting{Name: "work_mem",
		Value: "64MB", Queries: []string{"SELECT app_only FROM orders ORDER BY app_only",
			`SELECT qty FROM "Order Items"`}}, ladderNow)
	if r.Result != LadderPassed || !r.Passed() || r.Evidence["planned"] != 2 {
		t.Fatalf("record = %+v, want a pass that planned both targets", r)
	}
	if got := testdb.SessionPath(t, pinned); got != testdb.PinnedPath {
		t.Fatalf("search_path after the test = %q, want %q", got, testdb.PinnedPath)
	}
}

func TestFromSessionSetting_DecoyAndMissingTablesAreSkipped(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "verify_ladder_decoy")
	for q, want := range map[string]string{
		"SELECT sage_only FROM orders": `column "sage_only" does not exist`,
		"SELECT * FROM missing_orders": `relation "missing_orders" does not exist`,
	} {
		r := FromSessionSetting(t.Context(), pinned, SessionSetting{Name: "work_mem",
			Value: "64MB", Queries: []string{q}}, ladderNow)
		if r.Passed() || r.Result != LadderSkipped || len(r.Skipped) != 1 ||
			!strings.Contains(r.Skipped[0].Reason, want) {
			t.Fatalf("%q: record = %+v, want skipped with %q", q, r, want)
		}
	}
}
