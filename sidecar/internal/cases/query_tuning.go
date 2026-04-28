package cases

func queryTuningCandidate(f SourceFinding) (ActionCandidate, bool) {
	switch f.Category {
	case "query_work_mem_promotion":
		return roleWorkMemCandidate(f), true
	case "query_create_statistics":
		return createStatisticsCandidate(f), true
	case "query_parameterization":
		return parameterizedQueryCandidate(f), true
	default:
		return ActionCandidate{}, false
	}
}
