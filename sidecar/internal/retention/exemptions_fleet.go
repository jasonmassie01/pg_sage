package retention

// fleetLearningExemptions are the fleet learning tables: small current
// state, replaced every cycle, never a growing time series.
var fleetLearningExemptions = map[string]string{
	"fleet_fingerprint": "one row per fleet database, replaced every fleet learning " +
		"cycle; departed databases are pruned by the cycle",
	"fleet_leader_lease": "one row per fleet scope, current state",
	"fleet_outcome_digest": "per database and action shape counts, replaced every " +
		"fleet learning cycle; departed databases are pruned by the cycle",
}
