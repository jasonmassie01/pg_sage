package snapstore

// fullCategories are the categories stored in full every cycle, each for
// a stated reason (change-only catalog storage covers every other one the
// collector writes).
var fullCategories = map[string]string{
	"system": "one small object whose every member moves each cycle, read raw " +
		"(not through snapshot_data) by the forecaster and the verifier",
	"locks": "volatile rows with no stable identity (a pid is not unique); usually " +
		"empty or tiny",
	"replication": "a few replicas and slots whose LSNs move every cycle; a delta " +
		"would carry almost the whole document",
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
