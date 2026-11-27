package retention

// planExemptions: a plan's parts age out with their plan (plan-01a); the
// plan itself is purged by planPurgeRule, and an open plan is kept however
// old because the engine may still act on it. Its audit record (events and
// model passes, plan-01b) is chained and kept, like the other audit chains.
var planExemptions = map[string]string{
	"plan_step": "deleted with its plan (ON DELETE CASCADE); open plans are kept " +
		"however old, terminal ones age out with actions_days",
	"plan_approval": "deleted with its plan (ON DELETE CASCADE); the approval record " +
		"lives as long as the plan it binds",
	"plan_step_approval": "deleted with its plan step (ON DELETE CASCADE); a step's " +
		"approval lives as long as the step it binds (plan-03a)",
	"plan_event": "chained, append-only history of a plan (plan-01b); outlives its " +
		"plan, kept like fact_events",
	"plan_model_record": "chained, append-only model passes a plan was built from " +
		"(plan-01b), the input a later model is replayed on; outlives its plan",
}
