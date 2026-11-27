package snapstore

// fullCategories are the categories stored in full every cycle, each for
// a stated reason (change-only catalog storage covers every other one the
// collector writes).
var fullCategories = map[string]string{
	"system": "one small object whose every member moves each cycle, read raw " +
		"(not through snapshot_data) by the forecaster and the verifier",
	"locks": "volatile rows with no stable identity (a pid is not unique); a wait " +
		"graph bounded at 200 rows with redacted statement text, empty without waiters " +
		"or open transactions",
	"replication": "a few replicas and slots whose LSNs move every cycle; a delta " +
		"would carry almost the whole document",
	"subscriptions": "a few subscriptions (at most 200) whose LSNs and report times " +
		"move every cycle; a delta would carry almost the whole document",
	"workload_sessions": "session groups whose ages move every cycle and long sessions " +
		"keyed by a reused pid, bounded at 200 groups, 50 sessions (tags only, no " +
		"statement text) and 200 roles (market-08), with connection ages by state and " +
		"by application minute, bounded at 500 rows (evid-02b, review-02e)",
	"archiver": "the server's one pg_stat_archiver row (a few hundred bytes) whose " +
		"archive age and current WAL file move every cycle",
	"wait_events": "a few rows (active sessions by wait class) that change every cycle; " +
		"an idle database is an empty list",
}

// Coverage reports how the store keeps category: delta is true for a
// change-only category (a keyed catalog list or an object document); a
// category stored in full has the reason it is. Both are zero for a
// category the store has not decided on.
func Coverage(category string) (delta bool, reason string) {
	if _, ok := keyFields[category]; ok || objectCategories[category] {
		return true, ""
	}
	return false, fullCategories[category]
}
