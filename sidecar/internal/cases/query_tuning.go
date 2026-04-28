package cases

func queryTuningCandidate(f SourceFinding) (ActionCandidate, bool) {
	switch f.Category {
	case "query_work_mem_promotion":
		return roleWorkMemCandidate(f), true
	default:
		return ActionCandidate{}, false
	}
}
