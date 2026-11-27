package retention

// trancheSevenExemptions lists the T7 sage tables with time columns that
// the retention sweep does not age out (T7 test round 1): an approval
// record and current state bounded by a unique key.
var trancheSevenExemptions = map[string]string{
	"approval_prs": "approval requests opened as pull requests, one row per queued " +
		"change and database (unique); the record of how a change was approved",
	"notification_mutes": "current state, one row per database and class (unique); " +
		"a new mute of that class replaces the row",
}
