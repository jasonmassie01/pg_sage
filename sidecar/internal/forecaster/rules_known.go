package forecaster

// knownDays returns the days whose inputs are known, in order: a day
// where every snapshot left an input null must not read as 0 (forecast-07).
func knownDays(
	aggs []DaySystemAgg, unknown func(DaySystemAgg) bool,
) []DaySystemAgg {
	known := make([]DaySystemAgg, 0, len(aggs))
	for _, a := range aggs {
		if !unknown(a) {
			known = append(known, a)
		}
	}
	return known
}

// knownSizeDays returns the days with a measured database size.
func knownSizeDays(aggs []DaySystemAgg) []DaySystemAgg {
	return knownDays(aggs, func(a DaySystemAgg) bool { return a.DBSizeUnknown })
}

// knownConnectionDays returns the days with backend counts and
// max_connections both known.
func knownConnectionDays(aggs []DaySystemAgg) []DaySystemAgg {
	return knownDays(aggs, func(a DaySystemAgg) bool {
		return a.BackendsUnknown || a.MaxConnectionsUnknown
	})
}
