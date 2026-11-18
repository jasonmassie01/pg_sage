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

// sprawlCensusExemptions are the sprawl census tables (fleetpol-03): the
// census writer prunes them itself, in the transaction that writes a run.
var sprawlCensusExemptions = map[string]string{
	"sprawl_census_runs": "pruned by its writer: fleet.CensusDB.WriteCensus drops " +
		"runs older than 400 days (fleet.CensusRetention), one run per month",
	"sprawl_census": "a census run's rows; they go with their run (ON DELETE CASCADE)",
	"sprawl_activity": "current state, one row per observe-only member's newest " +
		"pg_stat_statements reading; a departed member's row is pruned by the census writer",
}
