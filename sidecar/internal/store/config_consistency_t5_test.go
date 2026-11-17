package store

// trancheFiveExcludedKeys are the T5 keys the registry marks restart-bound
// and not overridable, checked beside excludedExactKeys: lab-04a's clone rehearsal limits, lab-05a's clone create limit and
// full-copy opt-in (safety-critical), evid-01b's query rollup and
// evid-02a's workload pack retention (read when the workers start),
// lab-03b's canary stage wait, lab-01c's verifier method and expiry
// policy and lab-02's back-test bounds (safety-critical: an API write
// must never change how a change is judged or how fast a canary
// widens).
var trancheFiveExcludedKeys = map[string]bool{
	"clone.allow_full_copy":                     true,
	"clone.max_creates_per_hour":                true,
	"clone.rehearsal_statement_timeout_seconds": true,
	"clone.rehearsals_per_hour":                 true,
	"retention.query_rollup_daily_days":         true,
	"retention.query_rollup_hourly_days":        true,
	"retention.workload_packs_days":             true,
	"sre.autonomy.canary.stage_wait_hours":      true,
	"verify.backtest_max_runtime_minutes":       true,
	"verify.backtest_placebos":                  true,
	"verify.method":                             true,
	"verify.on_expired":                         true,
}
