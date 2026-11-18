package store

// trancheSixExcludedKeys are the T6 config keys that are YAML-only, with no
// API override (T6 round 1). Restore drills (drill-01*): the drill command
// runs programs on the sidecar host, so only someone with access to the
// host may set it, and the fleet cap is read when the leader starts. Sage
// Review (review-*): read when the review runtime starts (restart to
// change), and the review model's client (llm.review_llm.*, review-03a) is
// configured like llm.optimizer_llm, which is YAML-only too.
var trancheSixExcludedKeys = map[string]bool{
	"drill.command.destroy_argv":         true,
	"drill.command.env_allow":            true,
	"drill.command.restore_argv":         true,
	"drill.max_concurrent_per_fleet":     true,
	"llm.review_llm.api_key":             true,
	"llm.review_llm.cooldown_seconds":    true,
	"llm.review_llm.enabled":             true,
	"llm.review_llm.endpoint":            true,
	"llm.review_llm.fallback_to_general": true,
	"llm.review_llm.max_output_tokens":   true,
	"llm.review_llm.model":               true,
	"llm.review_llm.timeout_seconds":     true,
	"llm.review_llm.token_budget_daily":  true,
	"review.calendar_days":               true,
	"review.consensus":                   true,
	"review.enabled":                     true,
	"review.fleet_deep_max":              true,
	"review.gate.fixture_databases":      true,
	"review.gate.max_gap_points":         true,
	"review.gate.max_revert_pct":         true,
	"review.gate.min_reviews":            true,
	"review.grade_interval_hours":        true,
	"review.hash_identifiers":            true,
	"review.large_table_bytes":           true,
	"review.min_interval_hours":          true,
	"review.model_pass":                  true,
	"review.model_runs":                  true,
	"review.triage_model":                true,
	"review.window_days":                 true,
}
