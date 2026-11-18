package retention

// trancheSixExemptions are the T6 sage tables with time columns that the
// retention sweep does not age out by their own rule (T6 round 1): rows
// that go with a purged parent, current state, configuration, audit
// records, or a table its writer prunes.
var trancheSixExemptions = map[string]string{
	"review_candidate": "deleted with its review run (ON DELETE CASCADE from review_run, " +
		"400 days)",
	"review_model_run": "the model pass ledger of a review run; deleted with its run (ON " +
		"DELETE CASCADE from review_run)",
	"review_plan":   "deleted with its review run (ON DELETE CASCADE from review_run)",
	"review_advice": "deleted with its review run (ON DELETE CASCADE from review_run)",
	"plan_step_origin": "deleted with its plan step and plan (ON DELETE CASCADE from " +
		"plan_step and plan, which the plan rule ages out)",
	"review_model_authority": "current state, one row per model id; a revoked grant stays " +
		"as the audit record of who gave the model authority",
	"policy_sets": "configuration, one row per named policy set",
	"policy_set_events": "audit trail of policy set changes, one row per change a " +
		"person or the API made; kept like config_audit",
	"policy_exceptions": "current state, one row per database that overrides its set",
	"observe_sweeps": "current state, one row per observe-only member (ON DELETE " +
		"CASCADE from databases)",
	"observe_rounds": "pruned by its writer: store.DatabaseStore.FinishObserveRound deletes rounds " +
		"started more than 30 days ago",
	"exposure_remediations": "one row per approved exposure revoke, the record of what " +
		"PUBLIC lost that the rollback re-GRANT restores; rare and small",
}
