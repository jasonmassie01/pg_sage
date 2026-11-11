package main

// runbook is the docs/metrics.md section an alert's runbook_url links.
type runbook struct {
	Alert, Severity, Fires, Do string
}

// runbooks lists one entry per alert in deploy/prometheus/pg_sage.rules.yml,
// in the file's order; the tests keep the two in step.
var runbooks = []runbook{
	{"PgSageDown", "page",
		"Prometheus has not scraped a pg_sage target (`up == 0`) for 5 minutes.",
		"Check that the sidecar process or pod runs and that the scrape reaches " +
			"`/metrics` (the listener's `pg_sage_metrics_listener_up` is 0 when it " +
			"could not bind). The rule matches `job=\"pg_sage\"`; edit it if your " +
			"scrape job has another name."},
	{"PgSageDatabaseDown", "page",
		"`pg_sage_database_up` has been 0 for 5 minutes: the database is " +
			"unreachable or its status refresh fails.",
		"Open the database's page in the dashboard for the last error, check the " +
			"database and the network path, and the sidecar log for `[fleet]` " +
			"warnings. Nothing is collected or acted on while it is down."},
	{"PgSageCollectorStale", "ticket",
		"The collector's last snapshot is older than 3 intervals (180 s at the " +
			"default `collector.interval_seconds` of 60).",
		"Check `pg_sage_collector_breaker_state` (1 skipping, 2 dormant under " +
			"load) and `pg_sage_collector_persist_failures_total`. Raise the 180 " +
			"in the rule if you set a longer collector interval."},
	{"PgSageLLMCircuitOpen", "ticket",
		"The LLM circuit breaker has been open for 15 minutes; model-backed " +
			"features fall back to rules.",
		"Check the provider's status, the API key and `pg_sage_llm_tokens_used_today` " +
			"against `pg_sage_llm_tokens_budget_daily`; see the LLM setup guide."},
	{"PgSageActionFailed", "page",
		"An action failed in the last 15 minutes: its outcome counter rose, or " +
			"the database's first failure was recorded.",
		"Read the action log in the dashboard (Actions) for the error and what " +
			"was rolled back. pg_sage tries the change again on later cycles " +
			"until it has failed three times for that object, so fix the cause " +
			"or hold it with the emergency stop."},
	{"PgSageActionRolledBack", "ticket",
		"An action was rolled back in the last hour, by verification (a " +
			"regression) or by hand. A Prometheus with no history of the last hour " +
			"also reports earlier rollbacks once.",
		"Read the verification verdict in the action log. A rolled-back finding " +
			"is not applied again for `trust.rollback_cooldown_days` (7 by default)."},
	{"PgSageSelfBudgetExceeded", "ticket",
		"pg_sage has used more than its declared budget of a resource for 15 " +
			"minutes (`self_budget`).",
		"Compare `pg_sage_self_*` with `pg_sage_self_budget`; lengthen collector " +
			"or analyzer intervals, or raise the budget if the load is expected."},
	{"PgSageSLOPage", "page",
		"An SLO's error budget is burning at page rate (`pg_sage_slo_state" +
			"{state=\"page\"}`).",
		"Open the SLO in the dashboard for the burning window and the incidents " +
			"Sage SRE linked to it."},
	{"PgSageInvestigationBacklogOld", "ticket",
		"A Sage SRE investigation has been open for over an hour.",
		"Check `pg_sage_sre_investigations` by state: runs in `needs_evidence` wait " +
			"for a probe or a person, and `pg_sage_sre_probe_refusals_total` shows " +
			"probe slots running out."},
}
