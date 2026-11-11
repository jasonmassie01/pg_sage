package executor

// PruneCounts drops every /metrics counter of database: its parks, wait
// releases, record failures, action outcomes and verification verdicts,
// and the pools that count under its name. Call it once the database has
// left the process (removed by a fleet reload, deleted or renamed), so
// its series stop being exported. A database added again counts from zero.
func PruneCounts(database string) {
	if database == "" {
		return // never counted: an embedder's executor has no name
	}
	parkCounters.mu.Lock()
	pruneKeys(parkCounters.parks, database)
	pruneKeys(parkCounters.releases, database)
	parkCounters.mu.Unlock()
	recordFailureCounters.mu.Lock()
	pruneKeys(recordFailureCounters.counts, database)
	recordFailureCounters.mu.Unlock()
	pruneOutcomeCounts(database)
	unbindMetricsDatabase(database)
}

func pruneKeys(counts map[countKey]int64, database string) {
	for k := range counts {
		if k.database == database {
			delete(counts, k)
		}
	}
}
