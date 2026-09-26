package rca

// filterLogSignals drops log signals that do not belong to this engine:
//   - lines timestamped before the replay cutoff, so a restart does not
//     re-fire incidents from the replayed log tail (G1-B25);
//   - lines from another database of the same cluster, so fleet fanout
//     does not raise one database's errors on every database (G1-B11).
//
// Lines without a timestamp or without a database are kept: they cannot
// be attributed, and cluster-wide events (PANIC, disk full, archiver)
// concern every database. Caller holds e.mu.
func (e *Engine) filterLogSignals(signals []*Signal) []*Signal {
	out := make([]*Signal, 0, len(signals))
	replayed, foreign := 0, 0
	for _, s := range signals {
		if s == nil {
			continue
		}
		if !e.logReplayCutoff.IsZero() && !s.FiredAt.IsZero() &&
			s.FiredAt.Before(e.logReplayCutoff) {
			replayed++
			continue
		}
		if e.logDatabase != "" {
			if db := stringMetric(s, "database"); db != "" &&
				db != e.logDatabase {
				foreign++
				continue
			}
		}
		out = append(out, s)
	}
	if replayed > 0 || foreign > 0 {
		e.logFn("debug", "rca: ignored %d replayed and %d other-database "+
			"log signals", replayed, foreign)
	}
	return out
}
