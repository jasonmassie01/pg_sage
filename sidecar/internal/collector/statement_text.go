package collector

// StatementText is a statement's pg_stat_statements text as the latest
// snapshot holds it, for the SRE investigator's plan-only EXPLAIN: the
// explainer never loads pg_stat_statements' whole query-text file. A
// statement outside the snapshot's top queries is not cached (false).
func (c *Collector) StatementText(queryID int64) (string, bool) {
	if c == nil || queryID == 0 {
		return "", false
	}
	snap := c.LatestSnapshot()
	if snap == nil {
		return "", false
	}
	for _, q := range snap.Queries {
		if q.QueryID == queryID && q.Query != "" {
			return q.Query, true
		}
	}
	return "", false
}
