package retention

// planExemptions: a plan's parts age out with their plan (plan-01a); the
// plan itself is purged by planPurgeRule, and an open plan is kept however
// old because the engine may still act on it.
var planExemptions = map[string]string{
	"plan_step": "deleted with its plan (ON DELETE CASCADE); open plans are kept " +
		"however old, terminal ones age out with actions_days",
	"plan_approval": "deleted with its plan (ON DELETE CASCADE); the approval record " +
		"lives as long as the plan it binds",
}
