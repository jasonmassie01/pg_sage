package retention

// retentionExemptions documents sage tables with time columns that are
// intentionally NOT purged by age. A new time-series table must be added
// to purgeRules or here (enforced by a test).
var retentionExemptions = mergeExemptions(coreExemptions, agentExemptions,
	fleetLearningExemptions)

// coreExemptions are the exempt tables of the sage schema proper.
var coreExemptions = map[string]string{
	"action_outcome": "deleted with its action (ON DELETE CASCADE, actions_days)",
	"ask_messages":          "deleted with its conversation (ON DELETE CASCADE)",
	"auth_audit":            "security audit trail of SSO link, unlink and grant use",
	"chatops_identities":    "admin-managed mapping of chat users to accounts, current state",
	"chatops_replay":        "pruned by chatops on every callback (24 h replay window)",
	"config":                "current configuration, not a time-series",
	"config_audit":          "security audit trail of configuration changes",
	"crypto_meta":           "key metadata, not a time-series",
	"databases":             "fleet registry, not a time-series",
	"first_look": "bounded by its writer: firstlook.Store.Save keeps the newest 10 " +
		"reports per database",
	"onboarding": "one row per database, current state",
	"ha_identity":           "HA monitor history, current state, one row per monitor",
	"incident_avoided":      "value ledger; low volume, kept as evidence",
	"io_rate_sample":        "pruned by the IO sampler (verify.io_sample_retention_days)",
	"incidents":             "pruned by rca.PruneResolvedIncidents (resolved_at, findings_days)",
	"migration_run":         "low-volume migration evidence ledger",
	"notification_channels": "configuration",
	"notification_rules":    "configuration",
	"policy":                "standing policy, versioned configuration",
	"query_hints":           "active hints, current state",
	"recommendation_revision": "immutable revisions; deleted with their terminal " +
		"recommendation (ON DELETE CASCADE, actions_days)",
	"recommendation_transition": "immutable history; deleted with its terminal " +
		"recommendation (ON DELETE CASCADE, actions_days)",
	"rollout_run": "low-volume rollout evidence ledger",
	"runway_samples": "runway series; the runway monitor prunes them on every pass " +
		"(sre.runways.sample_retention_hours)",
	"rollout_instance": "per-database steps of a rollout run; low volume",
	// Sage SRE M7: earned-autonomy evidence. Purging by age would quietly
	// lower or erase the evidence promotions rest on ("no harmful outcome
	// ever"), so these are kept like the other evidence ledgers.
	"sre_family_autonomy":    "current autonomy level, one row per family x class",
	"sre_autonomy_proposals": "promotion proposals and their human decisions (audit)",
	"sre_autonomy_events":    "append-only autonomy history (audit); updates are refused",
	"sre_autonomy_outcomes":  "append-only live outcomes; promotion evidence",
	"sre_packet_reviews":     "operator shadow reviews; promotion evidence, one per review",
	"sre_game_days":          "game-day runs; low volume (at most one per interval_hours)",
	"trust_ledger_state": "one row per database: when its time-ramp autonomy was " +
		"grandfathered into the trust ledger, and the reconcile cursors",
	"trust_shadow_evidence": "shadow-mode promotion evidence, one row per counted " +
		"shadow decision (like sre_autonomy_outcomes)",
	"mcp_tokens": "admin-managed MCP API credentials; revoked and expired tokens " +
		"are kept as the audit trail of who could act through MCP",
	"config_derived_setting": "derived-settings state, one row per derived " +
		"key (self-configuration)",
	"config_derivation": "derivation ledger; rows only on a change, kept as the " +
		"evidence behind each derived value",
	"schema_baseline":        "current state, one row per object",
	"schema_findings":        "legacy table superseded by findings (v0.11); no writer",
	"sessions":               "expired sessions are deleted by auth's session cleaner",
	"slot_consumer_registry": "current state, one row per slot",
	"sre_change_events": "SRE change feed; aged out by its poller " +
		"(sre.change_events.retention_days)",
	"sre_change_feed_state": "SRE change feed cursors and snapshots, one row per source",
	"sre_service_slos":      "SLO definitions and current error-budget state, one row per SLO",
	"sre_sli_samples": "SLI counter samples; aged out by the SLO engine (the longest " +
		"SLO or burn window plus a day)",
	"sre_slo_transitions": "SLO state history; aged out by the SLO engine " +
		"(sre.timeline_retention_days)",
	"sre_action_proposals": "SRE action proposals; deleted with their investigation " +
		"(ON DELETE CASCADE) by sre retention",
	"sre_budget_reservations": "SRE model budget ledger; deleted with its investigation " +
		"by sre retention (sre.timeline_retention_days) unless the hold is unsettled",
	"sre_database_bindings": "stable SRE database identity, one row per database",
	"sre_deployments":       "the deployment identity, one row",
	"sre_events": "SRE event hash chain; deleted with its investigation by sre " +
		"retention (sre.timeline_retention_days)",
	"sre_evidence": "SRE evidence; aged out by sre retention (sre.evidence_retention_days, " +
		"pinned and live investigations kept, tombstoned)",
	"sre_hypotheses": "SRE hypotheses; deleted with their investigation by sre retention",
	"sre_investigations": "SRE investigations; aged out by sre retention " +
		"(sre.timeline_retention_days, pinned and live kept, tombstoned)",
	"sre_steps": "SRE investigation steps; deleted with their investigation by sre " +
		"retention",
	"sre_tombstones": "what sre retention deleted; one row per investigation and kind",
	"sre_runbooks":   "SRE runbooks, versioned configuration (retired, never deleted)",
	"sre_runbook_versions": "immutable SRE runbook versions and their signatures; the " +
		"audit trail of what was allowed to run",
	"sre_runbook_runs": "SRE runbook run history; deleted with its investigation " +
		"(ON DELETE CASCADE, sre.timeline_retention_days)",
	"sre_investigation_outcomes": "operator verdicts on SRE investigations; deleted with " +
		"their investigation (ON DELETE CASCADE, sre.timeline_retention_days)",
	"table_contract": "declared contracts, current state",
	"toil_model":     "model configuration",
	"users":          "accounts, not a time-series",
}

// agentExemptions: the agent_db_* tables are created on first use
// (agentdb.Store.Ensure). Append-only ones have rules (agent_rules.go);
// these back live objects, ledgers or audit trails.
var agentExemptions = map[string]string{
	"agent_identities":             "agent registry, current state",
	"agent_db_requests":            "database request ledger, one row per request; low volume",
	"agent_db_deployments":         "deployment registry: live and deleted databases",
	"agent_db_provider_configs":    "provider configuration",
	"agent_db_creation_receipts":   "evidence of created cloud resources, one per deployment",
	"agent_db_terraform_templates": "versioned configuration",
	"agent_db_blueprints":          "versioned configuration",
	"agent_db_size_profiles":       "configuration",
	"agent_db_recommendations":     "current recommendations, one row per kind per deployment",
	"agent_db_tuning_hints":        "current hints, one row per hint per deployment",
	"agent_db_cost_samples": "budget ledger: lifetime spend is checked against the " +
		"deployment's budget, so dropping samples would hide spend",
	"agent_db_backups":             "backups whose archives exist; restore evidence",
	"agent_db_audit":               "audit trail of agent database actions",
	"agent_db_deploy_requests":     "reviewed schema change requests (audit)",
	"agent_db_live_plans":          "plans live authorizations and receipts reference",
	"agent_db_live_authorizations": "authorizations of live cloud operations (audit)",
	"agent_db_live_receipts":       "receipts of live cloud operations (audit)",
	"agent_db_monitoring_policies": "configuration, one row per scope",
	"agent_db_monitoring_state":    "current schedule, one row per monitored target",
	"agent_db_schema_version":      "schema version marker, one row",
}

// mergeExemptions joins exemption maps.
func mergeExemptions(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
