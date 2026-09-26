package vectorlab

import "testing"

// Idle-in-transaction time is client latency between statements, not query
// time. Bounding it by the per-statement timeout (25 ms here) let a GC or
// scheduler pause kill the session with SQLSTATE 25P03 mid-experiment; the
// whole-run budget still stops a hung client from holding locks.
func TestSessionControlsBoundIdleByTotalBudget(t *testing.T) {
	got := map[string]string{}
	for _, s := range sessionControls(Manifest{StatementTimeoutMS: 25, TotalTimeoutMS: 5000}) {
		got[s[0]] = s[1]
	}
	want := map[string]string{
		"statement_timeout":                   "25ms",
		"lock_timeout":                        "25ms",
		"idle_in_transaction_session_timeout": "5000ms",
		"plan_cache_mode":                     "force_custom_plan",
		"search_path":                         "pg_catalog",
		"work_mem":                            "16MB",
	}
	if len(got) != len(want) {
		t.Fatalf("controls = %v, want %v", got, want)
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %q, want %q", name, got[name], value)
		}
	}
}
