package explain

import "encoding/json"

// EstimatedPlan reads a plan-only EXPLAIN (FORMAT JSON) that pg_sage did
// not run itself, such as an agent's from the agent broker (security-02),
// into the result shape Explain returns: node breakdown and estimated
// cost and the estimate-only rules floor (no catalog read), never timings.
// Nothing is cached or sent to an LLM. A plan the rules cannot read keeps
// empty lists and says so in the summary.
func EstimatedPlan(query string, planJSON json.RawMessage) *ExplainResult {
	result := buildResult(query, planJSON, false)
	result.Note = ""
	symptoms, _ := scanRules(result.PlanJSON, false, nil)
	addRuleEntries(result, symptoms)
	return result
}
