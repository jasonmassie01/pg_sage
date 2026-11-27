package tuner

import "context"

// deferralReason returns a non-empty string when the candidate
// should be skipped because at least one of the relations its
// plan touches has a pending index optimizer recommendation.
// The string identifies the matched relation for logging.
func (t *Tuner) deferralReason(
	ctx context.Context,
	c candidate,
	deferredTables map[string]bool,
) string {
	if len(deferredTables) == 0 {
		return ""
	}
	planJSON := t.deferralPlanJSON(ctx, c.QueryID)
	if planJSON == "" {
		return ""
	}
	reason, err := matchDeferred([]byte(planJSON), deferredTables)
	if err != nil {
		t.logFn("WARN",
			"tuner: extract relations queryid=%d: %v",
			c.QueryID, err)
		return ""
	}
	return reason
}

// matchDeferred is the pure form of the deferral check: given a
// plan JSON and a deferred-tables set, it returns a non-empty
// reason string when any relation in the plan is in the set.
// Pulled out so the gating logic is testable without a database.
func matchDeferred(
	planJSON []byte, deferredTables map[string]bool,
) (string, error) {
	if len(deferredTables) == 0 || len(planJSON) == 0 {
		return "", nil
	}
	rels, err := ExtractRelations(planJSON)
	if err != nil {
		return "", err
	}
	for rel := range rels {
		if deferredTables[rel] {
			return "table " + rel + " has pending index recommendation", nil
		}
	}
	return "", nil
}
