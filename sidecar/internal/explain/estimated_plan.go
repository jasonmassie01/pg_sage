package explain

import "encoding/json"

// EstimatedPlan reads a plan-only EXPLAIN (FORMAT JSON) that pg_sage did
// not run itself, such as an agent's from the agent broker (security-02),
// into the result shape Explain returns: node breakdown and estimated
// cost, never timings. Nothing is cached or sent to an LLM.
func EstimatedPlan(query string, planJSON json.RawMessage) *ExplainResult {
	result := buildResult(query, planJSON, false)
	result.Note = ""
	return result
}
