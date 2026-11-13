package retention

// trancheThreeExemptions are the T3 sage tables with time columns that the
// retention sweep does not age out: each is pruned by its own writer or is
// small current state, never a growing time series of pg_sage's.
var trancheThreeExemptions = map[string]string{
	"host_usage_hourly": "pruned by its writer: cost.RecordHourly drops a " +
		"resource's hours older than cost.UsageRetention (35 days, market-04)",
	"notification_outbox": "pruned by the notification dispatcher: finished " +
		"entries older than 7 days (outboxRetention, alerting-06)",
	"notification_state": "current alert state per database, dedup key and " +
		"channel; closed states older than 7 days are pruned with the outbox " +
		"(alerting-06)",
	"case_tickets": "one row per case linked to an external ticket (market-14): " +
		"low volume, the case's ticket of record; an unfinished claim is " +
		"released by its writer",
	"discovered_databases": "fleet discovery inventory, one row per discovered " +
		"resource and its lifecycle status (market-06); bounded by the fleet, " +
		"gone resources stay as the record of what was seen",
	"discovery_sources": "current state, one row per discovery source (market-06)",
	"user_database_grants": "access configuration, one row per user and " +
		"database (apiauth-08); deleted with its user",
}
