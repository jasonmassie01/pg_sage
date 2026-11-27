package retention

// The T6 time-series tables that age out by rule (T6 round 1, review-02f
// and review-05a). Every other T6 table with a time column is exempt with
// its reason in exemptions_t6.go.

// reviewForecastRule keeps 400 days of capacity projections (review-02f),
// as long as the review runs: a projection is settled 30 days after it was
// made, so an older row is history only.
var reviewForecastRule = purgeRule{table: "review_capacity_forecast",
	timeCol: "created_at", days: 400}

// reviewGateRule keeps 400 days of model-gate evaluations (review-05a), one
// row per model, rule and hourly tick. A row a person approved or that
// records a revocation is the audit record of who gave or took the model's
// authority, so it is never purged; the live grant itself is in
// sage.review_model_authority, which no rule touches.
var reviewGateRule = purgeRule{table: "review_gate", timeCol: "evaluated_at", days: 400,
	extra: "AND approved_by IS NULL AND revoked_at IS NULL"}
