package cases

func queryTuningCandidate(f SourceFinding) (ActionCandidate, bool) {
	switch f.Category {
	case "slow_query", "high_total_time":
		return investigateQueryCandidate(f), true
	case "query_work_mem_promotion":
		return roleWorkMemCandidate(f), true
	case "query_tuning":
		if f.RecommendedSQL != "" {
			return applyQueryHintCandidate(f), true
		}
		return investigateQueryCandidate(f), true
	case "query_create_statistics":
		return createStatisticsCandidate(f), true
	case "query_parameterization":
		return parameterizedQueryCandidate(f), true
	default:
		return ActionCandidate{}, false
	}
}
