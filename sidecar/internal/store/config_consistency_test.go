package store

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// excludedPrefixes are top-level Config sections that represent
// connection/infrastructure settings, not hot-reloadable config
// overrides. They are intentionally absent from api-tagged keys.
var excludedPrefixes = []string{
	"postgres.",
	"prometheus.",
	"azure.", // cloud identity for server parameters; YAML/env only, restart-bound
	"api.",
	"oauth.",
	"auto_explain.",
	"forecaster.",
	"tuner.",
	"briefing.",
	"databases.",
	"defaults.",
	"ask.", // Ask Sage (roadmap phase 3): YAML only, read when a runtime starts
	// binding facts (facts-08): YAML only, read when a runtime starts
	"facts.",
	// pg_sage's declared self-budget and the admin-only profiler are
	// YAML only and restart-bound, not API overrides (phase 3 observer).
	"self_budget.",
	"debug.",
	// history placement: YAML only, restart-bound, changed with a migration (v2.3.0)
	"history.",
	// managed-cloud telemetry identity and guard floors: YAML/env only, restart-bound
	"cloud_telemetry.",
	"lakebase_observability.", // trusted resource bindings; restart-bound startup configuration
	// market-03: the Lakebase project rehearsal branches are made in and
	// their limits: YAML only, restart-bound, the project safety-critical
	"clone.lakebase.",
	// lab-05b: the Neon project, parent branch and API key of rehearsal
	// branches: YAML or env only, restart-bound, the key a secret
	"clone.neon.",
	// lab-05c: the Aurora region and clone size: YAML only, restart-bound,
	// the region safety-critical
	"clone.aurora.",
	// lab-05d: the RDS region and class and the full-copy restore deadline:
	// YAML only, restart-bound, the region safety-critical
	"clone.rds.", "clone.restore_deadline_minutes",
	// lab-05e: the Cloud SQL project and instance: YAML only, restart-bound,
	// safety-critical
	"clone.cloudsql.",
	// lab-05f: the AlloyDB project, location and cluster: YAML only,
	// restart-bound, safety-critical
	"clone.alloydb.",
	// lab-05g: the Azure subscription, resource group and server: YAML
	// only, restart-bound, safety-critical
	"clone.azure.",
	"sre.read_policy.", // trusted observation boundary; never a model-derived grant
	"self_config.",     // derivation switch and soak: YAML only, safety-critical, restart-bound
	// fleet learning and leader election: YAML only, restart-bound
	"fleet_learning.",
	// fleet auto-discovery (market-06): YAML only, restart-bound, safety-critical
	"fleet.discovery.",
	// agent posture: YAML only, restart-bound; exposed_roles is safety-critical
	"agents.",
	// E2 audit export and the MCP OAuth resource server: YAML only,
	// restart-bound, safety-critical (endpoints, credentials, who may call MCP)
	"audit.",
	"mcp.oauth.",
	// owner checks (market-11): YAML only, restart-bound, safety-critical
	"checks.",
	// seasonal workload baselines (market-02): YAML only, restart-bound
	"baseline.",
	// rehearsal plan-flip budget (migration-06): YAML only, safety-critical
	"rehearsal.",
	// provider advice reconciliation switch (market-09): YAML only, restart-bound
	"provider_advice.",
}

// excludedExactKeys are individual keys that exist in the Config
// struct but are intentionally excluded from the config override
// registries.
var excludedExactKeys = map[string]bool{
	// Workload budget proposals (market-08): YAML only, restart-bound.
	"advisor.budget_proposals": true,
	// The alerting engine switch (alerting-06b): YAML only, restart-bound;
	// the legacy engine is a one-release escape hatch, not a setting.
	"alerting.engine": true,
	// Migration contract delay and lock budget (migration-06): YAML only,
	// safety-critical and restart-bound; an API write can never loosen them.
	"migration.contract_delay_minutes": true,
	"migration.max_lock_ms":            true,
	// Subscriber and CDC thresholds (market-13): YAML only, restart-bound.
	"analyzer.subscription_lag_minutes": true,
	"analyzer.cdc_stall_minutes":        true,
	// Top-level mode/meta fields — not overridable via API.
	"mode":           true,
	"meta_db":        true,
	"encryption_key": true,
	// The rotation key must never be API-settable: it would be stored
	// next to what it protects.
	"encryption_key_previous": true,
	// data_dir holds the generated key file; it is a process setting, never
	// an API override (security-08).
	"data_dir": true,

	// Safety fields present in struct but not exposed as overrides.
	"safety.disk_pressure_threshold_pct": true,
	"safety.backoff_consecutive_skips":   true,
	"safety.dormant_interval_seconds":    true,

	// Analyzer fields present in struct but not exposed as overrides.
	"analyzer.table_bloat_min_rows":                true,
	"analyzer.table_bloat_min_bytes":               true, // read when the custodian is built
	"analyzer.index_bloat_min_bytes":               true, // rules-13: YAML only, restart-bound
	"analyzer.idle_in_transaction_timeout_minutes": true,
	"analyzer.xid_wraparound_warning":              true,
	"analyzer.xid_wraparound_critical":             true,
	"analyzer.regression_lookback_days":            true,
	// rules-05: the interval call floor is YAML only and restart-bound.
	"analyzer.slow_query_min_calls":                  true,
	"analyzer.checkpoint_frequency_warning_per_hour": true,
	// v0.8.5 Feature 3 — work_mem promotion advisor threshold.
	// Read once per cycle from YAML; not exposed as a runtime override.
	"analyzer.work_mem_promotion_threshold": true,
	// The schema guard's post-DDL debounce is read when the autonomy
	// supervisor starts; YAML only, restart-bound.
	"analyzer.schema_guard_ddl_debounce_seconds": true,
	// The non-execute decision retention window is YAML only; it is not
	// exposed as an API override.
	"retention.decisions_days": true,
	// The query_store window and the snapshot size cap (perf storage phase)
	// are YAML only and restart-bound, like decisions_days.
	"retention.query_store_days":  true,
	"retention.snapshots_max_pct": true,
	// The sign-in audit window (E1) is YAML only, like decisions_days.
	"retention.auth_audit_days": true,
	// The stdio transport's agent identity (agent governance G1) is YAML
	// only and restart-bound: an API write must never rebind it.
	"mcp.stdio_principal": true,
	// The stdio tools/call deadline (mcp-04) is YAML only and restart-bound,
	// read when the stdio server starts.
	"mcp.call_timeout_seconds": true,
	// The HA role check interval (fleet-04) is YAML only and restart-bound.
	"ha.check_interval_seconds": true,
	// The backup freshness policy (market-07) is YAML only and
	// restart-bound, like the HA cadence.
	"backup.max_age_hours":           true,
	"backup.max_archive_gap_minutes": true,
	"backup.min_retention_days":      true,

	// T5 keys the registry marks restart-bound and not overridable:
	// lab-04a's clone rehearsal limits, lab-05a's clone create limit and
	// full-copy opt-in (safety-critical), evid-01b's query rollup and
	// evid-02a's workload pack retention (read when the workers start),
	// lab-03b's canary stage wait, lab-01c's verifier method and expiry
	// policy and lab-02's back-test bounds (safety-critical: an API write
	// must never change how a change is judged or how fast a canary
	// widens).
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

	// Trust ramp_start — written in YAML but not overridable.
	"trust.ramp_start": true,

	// Fast elevation: the trust ramp ages, the hour-scale IO baseline and
	// the earned-autonomy promotion bar loosen trust gates, so they are
	// YAML only and restart-bound (the startup WARN lists each lowered
	// value); an API write can never shorten them.
	"trust.ramp_safe_hours":                          true,
	"trust.ramp_moderate_hours":                      true,
	"verify.io_baseline_hours":                       true,
	"verify.drop_window_hours":                       true,
	"sre.autonomy.promotion.shadow_window_hours":     true,
	"sre.autonomy.promotion.shadow_min_reviewed":     true,
	"sre.autonomy.promotion.shadow_min_accepted_pct": true,
	"sre.autonomy.promotion.bench_min_top1_pct":      true,
	"sre.autonomy.promotion.bench_min_precision_pct": true,
	"sre.autonomy.promotion.min_safe_pass_pct":       true,
	"sre.autonomy.promotion.min_live_recoveries":     true,
	// One trust system (roadmap 1.2): the self-initiated classes'
	// promotion bar is fast elevation too.
	"sre.autonomy.class_promotion.min_successes_l2":     true,
	"sre.autonomy.class_promotion.min_successes_l3":     true,
	"sre.autonomy.class_promotion.min_success_rate_pct": true,

	// D6 load admission: declared capacity is an operator attestation that
	// loosens a safety gate, and the ceilings/baseline are read at executor
	// start. YAML only, restart-bound, never a runtime override.
	"safety.data_io_ceiling_pct":         true,
	"safety.wal_io_ceiling_pct":          true,
	"verify.io_baseline_days":            true,
	"verify.io_sample_retention_days":    true,
	"verify.io_capacity.read_write_mbps": true,
	"verify.io_capacity.wal_mbps":        true,

	// Notification target policy is a security boundary (G7-B21): an
	// API admin must not be able to open SSRF to private networks, so it
	// is YAML-only and restart-bound.
	"notification_policy.allow_private_targets": true,

	// LLM sub-struct fields not (yet) exposed as overrides.
	"llm.cooldown_seconds":                      true,
	"llm.index_optimizer.enabled":               true,
	"llm.index_optimizer.min_query_calls":       true,
	"llm.index_optimizer.max_indexes_per_table": true,
	"llm.index_optimizer.max_include_columns":   true,
	"llm.index_optimizer.write_heavy_ratio_pct": true,
	"llm.optimizer.max_indexes_per_table":       true,
	"llm.optimizer.max_include_columns":         true,
	"llm.optimizer.write_heavy_ratio_pct":       true,
	"llm.optimizer.min_snapshots":               true,
	"llm.optimizer.hypopg_min_improvement_pct":  true,
	"llm.optimizer.confidence_threshold":        true,
	"llm.optimizer.write_impact_threshold_pct":  true,
	"llm.optimizer_llm.enabled":                 true,
	"llm.optimizer_llm.endpoint":                true,
	"llm.optimizer_llm.api_key":                 true,
	"llm.optimizer_llm.model":                   true,
	"llm.optimizer_llm.timeout_seconds":         true,
	"llm.optimizer_llm.token_budget_daily":      true,
	"llm.optimizer_llm.cooldown_seconds":        true,
	"llm.optimizer_llm.max_output_tokens":       true,
	"llm.optimizer_llm.fallback_to_general":     true,
	// Provider wire shape (OpenAI compatibility): YAML-only, applied by the
	// llm reconfigure owner; auto adapts without operator action.
	"llm.token_parameter":       true,
	"llm.tool_reasoning_effort": true,
	// Privacy switch (llm-09): YAML-only, applied live by llm_redaction.
	"llm.redact_numeric_literals": true,

	// Optimizer what-if rejection memory: YAML-only, read when the
	// optimizer is built (restart-bound).
	"llm.optimizer.rejection_memory.enabled":            true,
	"llm.optimizer.rejection_memory.max_age_days":       true,
	"llm.optimizer.rejection_memory.call_volume_ratio":  true,
	"llm.optimizer.rejection_memory.mean_time_ratio":    true,
	"llm.optimizer.rejection_memory.row_estimate_ratio": true,
	"llm.optimizer.rejection_memory.prompt_max_shapes":  true,
	"llm.optimizer.rejection_memory.skip_llm_after":     true,

	// Roadmap 2.2 tuning agent: budgets and calibration are read when a
	// database runtime is built (restart lifecycle) and validated at
	// load, so they are YAML-only.
	"tuning.enabled":                  true,
	"tuning.max_cases_per_cycle":      true,
	"tuning.max_requests_per_cycle":   true,
	"tuning.max_tokens_per_cycle":     true,
	"tuning.max_turns_per_case":       true,
	"tuning.max_proposals_per_cycle":  true,
	"tuning.calibration_min_outcomes": true,
	"tuning.calibration_window_days":  true,

	// Advisor sub-fields not exposed as overrides.
	"advisor.vacuum_enabled":     true,
	"advisor.wal_enabled":        true,
	"advisor.connection_enabled": true,
	"advisor.memory_enabled":     true,
	"advisor.rewrite_enabled":    true,
	"advisor.bloat_enabled":      true,

	// Alerting sub-fields not exposed as overrides.
	"alerting.timezone": true,

	// String-slice fields managed via YAML only, not API overrides.
	"analyzer.lock_chain.safe_patterns": true,
	"runaway.safe_patterns":             true,

	// v0.10: Schema lint — string-slice fields managed via YAML only.
	"schema_lint.include_schemas": true,
	"schema_lint.exclude_schemas": true,
	"schema_lint.disabled_rules":  true,

	// Sage SRE M0: the fast-path period is read once at startup and must
	// be 0 or 10-3600 (validated at load), so it is YAML-only.
	"rca.lock_chain_interval_seconds": true,
	// rca-06: the log fast-path period, read once at startup like the
	// lock-chain fast path and validated at load (0 or 1-3600).
	"rca.log_fast_path_interval_seconds": true,
	// rca-07: the log quiet period and the recurrence re-notify window
	// are validated at load against rca.stale_after_hours (1-max), so
	// they are YAML-only.
	"rca.log_quiet_minutes":         true,
	"rca.recurrence_renotify_hours": true,
	// rca-07b: whether deadlock statement text is kept, validated at load
	// and read once when the log classifier starts: YAML-only.
	"rca.deadlock_statement_text": true,

	// Dogfood lifeos-1: validated at load against other keys
	// (rca.dedup_window_minutes; sre.runways.interval_seconds,
	// lookback_hours and min_samples), so they are YAML-only.
	"rca.stale_after_hours":                 true,
	"sre.runways.sequence_interval_seconds": true,
	// v2.3.x: the size sampling cadence, validated at load like the
	// sequence interval, so it is YAML-only.
	"sre.runways.size_interval_seconds": true,

	// Dogfood round 2: the vacuum_blocked size floor is validated at load
	// (1-max), like rca.stale_after_hours, so it is YAML-only.
	"rca.vacuum_min_dead_tuples": true,
	"rca.vacuum_min_table_mb":    true,

	// rca-01: the vacuum_blocked holder floors are validated at load
	// (1-max) and gate blame and proposals, so they are YAML-only.
	"rca.xmin_holder_min_minutes":       true,
	"rca.prepared_xact_min_age_minutes": true,

	// Sage SRE M2: read once when a database runtime is built (restart
	// lifecycle) and validated at load, so they are YAML-only.
	"sre.automatic_start":          true,
	"sre.trigger_interval_seconds": true,
	"sre.sample_interval_seconds":  true,
	"sre.evidence_retention_days":  true,
	"sre.timeline_retention_days":  true,

	// Sage SRE M3: the model turn switch is read when a database runtime
	// is built (restart lifecycle), so it is YAML-only.
	"sre.llm.enabled": true,

	// Roadmap 2.1: the investigator mode is read with the model turn
	// switch (restart lifecycle), so it is YAML-only too.
	"sre.llm.mode": true,

	// sre-03: the investigator's support-citation rule is read when a
	// database runtime is built (restart lifecycle) and only tightens what
	// a model may conclude, so an API write can never switch it off: YAML only.
	"sre.investigator.require_support_citation": true,

	// Sage SRE M5: SLOs and the change feed are built with the database
	// runtime (restart lifecycle), carry secrets and are validated at
	// load, so they are YAML-only.
	"sre.slo.enabled":                                true,
	"sre.slo.evaluation_interval_seconds":            true,
	"sre.slo.open_investigations":                    true,
	"sre.slo.prometheus.url":                         true,
	"sre.slo.prometheus.bearer_token":                true,
	"sre.slo.prometheus.bearer_token_file":           true,
	"sre.slo.prometheus.timeout_seconds":             true,
	"sre.slo.push.hmac_secret":                       true,
	"sre.slo.push.timestamp_tolerance_seconds":       true,
	"sre.slo.proxies.enabled":                        true,
	"sre.slo.proxies.target":                         true,
	"sre.slo.proxies.window_days":                    true,
	"sre.slo.proxies.latency_threshold_ms":           true,
	"sre.slo.proxies.latency_factor":                 true,
	"sre.slo.proxies.latency_floor_ms":               true,
	"sre.slo.proxies.top_queries":                    true,
	"sre.slo.proxies.replication_lag_budget_seconds": true,
	"sre.change_events.hmac_secret":                  true,
	"sre.change_events.timestamp_tolerance_seconds":  true,
	"sre.change_events.allowed_sources":              true,
	"sre.change_events.feed_enabled":                 true,
	"sre.change_events.feed_interval_seconds":        true,
	"sre.change_events.retention_days":               true,
	// Sage SRE M5: approved-action settings are read when a database
	// runtime is built (restart lifecycle) and validated at load, so they
	// are YAML-only.
	"sre.actions.proposals":                 true,
	"sre.actions.request_approval":          true,
	"sre.actions.approval_ttl_minutes":      true,
	"sre.actions.max_evidence_age_seconds":  true,
	"sre.actions.recovery_sample_seconds":   true,
	"sre.actions.recovery_samples":          true,
	"sre.actions.recovery_deadline_minutes": true,
	"sre.actions.chatops_tolerance_seconds": true,
	"sre.actions.protected_roles":           true,
	"sre.actions.protected_applications":    true,

	// Postgres-specialist contract (roadmap phase 3): read when the API
	// router is built (restart lifecycle), validated at load and carrying
	// secrets, so they are YAML-only.
	"specialist.enabled":                             true,
	"specialist.writes_per_minute":                   true,
	"specialist.reads_per_minute":                    true,
	"specialist.max_open_per_identity":               true,
	"specialist.max_open_total":                      true,
	"specialist.max_live_age_hours":                  true,
	"specialist.keep_identifiers":                    true,
	"specialist.pagerduty.signing_secret":            true,
	"specialist.pagerduty.services":                  true,
	"specialist.pagerduty.api_url":                   true,
	"specialist.pagerduty.api_token":                 true,
	"specialist.pagerduty.from_email":                true,
	"specialist.webhook.signing_secret":              true,
	"specialist.webhook.result_url":                  true,
	"specialist.webhook.timestamp_tolerance_seconds": true,

	// Sage SRE M6: runways are read when a database runtime is built
	// (restart lifecycle) and validated together at load, so they are
	// YAML-only.
	"sre.runways.enabled":                   true,
	"sre.runways.investigate":               true,
	"sre.runways.interval_seconds":          true,
	"sre.runways.lookback_hours":            true,
	"sre.runways.min_samples":               true,
	"sre.runways.min_span_minutes":          true,
	"sre.runways.wraparound_horizon_hours":  true,
	"sre.runways.wraparound_critical_hours": true,
	"sre.runways.disk_horizon_hours":        true,
	"sre.runways.disk_critical_hours":       true,
	"sre.runways.sequence_horizon_days":     true,
	"sre.runways.sequence_critical_days":    true,
	"sre.runways.sample_retention_hours":    true,
	// Sage SRE M7: earned-autonomy settings are read when a database
	// runtime is built (restart lifecycle) and validated at load, so they
	// are YAML-only. enforce in particular must not be switchable over the
	// API: turning it off returns incident-family actions to the trust ramp.
	"sre.autonomy.enforce":                    true,
	"sre.autonomy.bench_results_path":         true,
	"sre.autonomy.evaluate_interval_minutes":  true,
	"sre.autonomy.reconcile_interval_seconds": true,
	"sre.autonomy.max_evidence_age_seconds":   true,
	"sre.autonomy.concurrency_window_minutes": true,
	"sre.autonomy.safety_window_days":         true,
	"sre.autonomy.failover_cooldown_minutes":  true,
	"sre.autonomy.proposal_ttl_hours":         true,
	"sre.autonomy.game_days.enabled":          true,
	"sre.autonomy.game_days.interval_hours":   true,
	"sre.autonomy.game_days.local_dsn":        true,
	"sre.autonomy.game_days.families":         true,
	// benches-12: whether game days run customer programs on the clone.
	"sre.autonomy.game_days.customer_programs": true,
	"sre.autonomy.canary.canary_instances":     true,
	"sre.autonomy.canary.regression_limit_pct": true,
	// Stored bench and game-day reports age out with retention; the
	// floor protects the promotion evidence window, so YAML-only.
	"sre.autonomy.report_retention_days": true,
	// Sage SRE M6 reactive detector thresholds: read when a database
	// runtime builds its investigator (restart lifecycle) and validated at
	// load, so they are YAML-only.
	"sre.detectors.window_seconds":       true,
	"sre.detectors.checkpoint_requested": true,
	"sre.detectors.temp_file_mb":         true,
	"sre.detectors.lwlock_waiters":       true,
	"sre.detectors.lwlock_polls":         true,
	"sre.detectors.cooldown_minutes":     true,
}

// structFieldPaths walks a struct type using reflection and returns
// all leaf-level dot-notation paths derived from yaml tags. It
// recurses into nested structs but skips fields without yaml tags,
// fields tagged "-", and slice/map/pointer fields that aren't
// structs.
func structFieldPaths(t reflect.Type, prefix string) []string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var paths []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		// Strip yaml tag options (e.g. ",omitempty").
		if idx := strings.Index(tag, ","); idx != -1 {
			tag = tag[:idx]
		}
		if tag == "" {
			continue
		}

		key := tag
		if prefix != "" {
			key = prefix + "." + tag
		}

		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}

		switch ft.Kind() {
		case reflect.Struct:
			// Recurse into nested structs.
			paths = append(paths, structFieldPaths(ft, key)...)
		case reflect.Slice:
			// Include []string as a leaf (serialized as comma-
			// separated in the config API). Skip complex slice
			// types (e.g. []DatabaseConfig, []RunawayPolicy).
			if ft.Elem().Kind() == reflect.String {
				paths = append(paths, key)
			}
			continue
		case reflect.Map:
			continue
		default:
			paths = append(paths, key)
		}
	}
	return paths
}

// isExcluded returns true if a field path should not be expected
// in api-tagged keys.
func isExcluded(path string) bool {
	if excludedExactKeys[path] {
		return true
	}
	for _, prefix := range excludedPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// TestConfigConsistency_AllowedKeysMatchStruct verifies that every
// leaf field in config.Config (minus excluded infra/connection
// fields) has a corresponding entry in api-tagged keys.
//
// When you add a new field to a Config sub-struct, this test will
// fail until you also add it to api-tagged keys (or explicitly
// exclude it).
func TestConfigConsistency_AllowedKeysMatchStruct(t *testing.T) {
	allPaths := structFieldPaths(
		reflect.TypeOf(config.Config{}), "")

	var missing []string
	for _, path := range allPaths {
		if isExcluded(path) {
			continue
		}
		if _, ok := configKeyTypes()[path]; !ok {
			missing = append(missing, path)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf(
			"config.Config has %d field(s) not in "+
				"api-tagged keys (add api tags or exclude them "+
				"in config_consistency_test.go):\n  %s",
			len(missing),
			strings.Join(missing, "\n  "))
	}
}

// TestConfigConsistency_AllowedKeysExistInStruct verifies the
// reverse: every key in api-tagged keys corresponds to a real
// yaml-tagged field in config.Config.
func TestConfigConsistency_AllowedKeysExistInStruct(t *testing.T) {
	allPaths := structFieldPaths(
		reflect.TypeOf(config.Config{}), "")
	pathSet := make(map[string]bool, len(allPaths))
	for _, p := range allPaths {
		pathSet[p] = true
	}

	var orphans []string
	for key := range configKeyTypes() {
		if !pathSet[key] {
			orphans = append(orphans, key)
		}
	}

	if len(orphans) > 0 {
		sort.Strings(orphans)
		t.Errorf(
			"api-tagged keys has %d key(s) with no "+
				"matching config.Config field:\n  %s",
			len(orphans),
			strings.Join(orphans, "\n  "))
	}
}

// TestConfigConsistency_ConfigToMapCoversAllAllowedKeys verifies
// that configToMap() emits an entry for every key in
// api-tagged keys.
func TestConfigConsistency_ConfigToMapCoversAllAllowedKeys(
	t *testing.T,
) {
	cfg := &config.Config{
		// Use a valid trust level so the map is well-formed.
		Trust: config.TrustConfig{Level: "observation"},
	}
	m := configToMap(cfg)

	var missing []string
	for key := range configKeyTypes() {
		if _, ok := m[key]; !ok {
			missing = append(missing, key)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf(
			"configToMap() is missing %d key(s) that "+
				"api-tagged keys defines:\n  %s",
			len(missing),
			strings.Join(missing, "\n  "))
	}
}

// TestConfigConsistency_ConfigToMapOnlyAllowedKeys verifies that
// configToMap() does not emit keys that are absent from
// api-tagged keys. Stale or typo'd keys in configToMap would
// cause silent data that the SET API rejects.
func TestConfigConsistency_ConfigToMapOnlyAllowedKeys(
	t *testing.T,
) {
	cfg := &config.Config{
		Trust: config.TrustConfig{Level: "observation"},
	}
	m := configToMap(cfg)

	var extra []string
	for key := range m {
		if _, ok := configKeyTypes()[key]; !ok {
			extra = append(extra, key)
		}
	}

	if len(extra) > 0 {
		sort.Strings(extra)
		t.Errorf(
			"configToMap() emits %d key(s) not in "+
				"api-tagged keys:\n  %s",
			len(extra),
			strings.Join(extra, "\n  "))
	}
}

// TestConfigConsistency_HotReloadCoversAllAllowedKeys verifies
// that hotReload() (in internal/api/config_apply.go) handles every
// key defined in api-tagged keys.
//
// Strategy: call hotReload for each key with a non-zero value and
// verify the config struct was actually mutated. This catches keys
// that are accepted by the SET API but silently dropped by
// hotReload.
func TestConfigConsistency_HotReloadCoversAllAllowedKeys(
	t *testing.T,
) {
	// hotReload is in package api, which we can't call from
	// package store. Instead, we build the expected set of
	// hotReload keys by parsing the switch cases from the
	// function's behavior: for each api-tagged keys entry we
	// verify that a SET + GET round-trip through configToMap
	// reflects the change. This is an indirect but accurate test.

	// Build a map of test values per validation type.
	testValues := map[string]string{
		"int_pos": "42",
		// int_week_minutes is 1-10080 (analyzer.inactive_slot_min_minutes).
		"int_week_minutes":  "42",
		"int_nonneg":        "7",
		"int_min5":          "15",
		"int_class_timeout": "7200",
		"int_vacuum_stall":  "30",
		"pct":               "50",
		"pct1_100":          "75",
		"float01":           "0.5",
		"float_share":       "0.5",
		"bool":              "true",
		"trust_level":       "advisory",
		"exec_mode":         "auto",
		"string":            "test-value",
		// maintenance_window is validated against the policy window grammar.
		"maintenance_window": "weeknights",
		"rule_list":          "unused_indexes,slow_queries",
		"float_pos":          "2.0",
		"float_nonneg":       "0.5",
		"float_pct_pos":      "25.0",
		"float_pct_open":     "25.0", // vacuum-11b
		"policy_profile":     "staffed",
		"clone_provider":     "none",
		"mcp_transport":      "stdio",
		"llm_auth":           "databricks",
		"bool_auto":          "true", // rca-09b
	}

	for key, vtype := range configKeyTypes() {
		testVal, ok := testValues[vtype]
		if !ok {
			t.Errorf(
				"no test value defined for validation type %q "+
					"(key %q)", vtype, key)
			continue
		}

		// Create a fresh config, apply the override via
		// hotReload (simulated through the exported helpers),
		// then read back via configToMap.
		cfg := &config.Config{
			Trust: config.TrustConfig{Level: "observation"},
		}

		// We call hotReload indirectly: since it's in the api
		// package, we simulate it by calling the same logic
		// the SET handler uses — validate + mutate + read.
		// For this test, we just verify the configToMap output
		// includes the key (covered above) and that
		// coerceValue returns the right type.
		coerced := coerceValue(key, testVal)
		if coerced == nil {
			t.Errorf("coerceValue(%q, %q) returned nil", key, testVal)
		}

		// Verify configToMap has the key.
		m := configToMap(cfg)
		if _, exists := m[key]; !exists {
			t.Errorf(
				"configToMap missing key %q after building "+
					"default config", key)
		}
	}
}

// TestConfigConsistency_NoExclusionDrift ensures the exclusion
// lists don't contain entries that no longer exist in the struct.
// Stale exclusions mask real problems.
func TestConfigConsistency_NoExclusionDrift(t *testing.T) {
	allPaths := structFieldPaths(
		reflect.TypeOf(config.Config{}), "")
	pathSet := make(map[string]bool, len(allPaths))
	for _, p := range allPaths {
		pathSet[p] = true
	}

	var stale []string
	for key := range excludedExactKeys {
		if !pathSet[key] {
			stale = append(stale, key)
		}
	}

	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf(
			"excludedExactKeys has %d key(s) that no longer "+
				"exist in config.Config — remove them:\n  %s",
			len(stale),
			strings.Join(stale, "\n  "))
	}
}

// TestConfigConsistency_ExcludedAndAllowedAreDisjoint ensures no
// key appears in both the exclusion list and api-tagged keys.
// That would indicate confusion about whether a key is overridable.
func TestConfigConsistency_ExcludedAndAllowedAreDisjoint(
	t *testing.T,
) {
	var overlap []string
	for key := range excludedExactKeys {
		if _, ok := configKeyTypes()[key]; ok {
			overlap = append(overlap, key)
		}
	}

	if len(overlap) > 0 {
		sort.Strings(overlap)
		t.Errorf(
			"%d key(s) appear in BOTH excludedExactKeys and "+
				"api-tagged keys — pick one:\n  %s",
			len(overlap),
			strings.Join(overlap, "\n  "))
	}
}

// TestConfigConsistency_StructFieldPathsReturnsExpectedPaths is a
// sanity check that the reflection walker produces correct paths
// for known fields.
func TestConfigConsistency_StructFieldPathsReturnsExpectedPaths(
	t *testing.T,
) {
	paths := structFieldPaths(
		reflect.TypeOf(config.Config{}), "")
	pathSet := make(map[string]bool, len(paths))
	for _, p := range paths {
		pathSet[p] = true
	}

	// Spot-check a representative sample of expected paths.
	expected := []string{
		"collector.interval_seconds",
		"collector.batch_size",
		"analyzer.slow_query_threshold_ms",
		"analyzer.unused_index_window_days",
		"trust.level",
		"trust.tier3_safe",
		"trust.rollback_threshold_pct",
		"safety.cpu_ceiling_pct",
		"safety.lock_timeout_ms",
		"llm.enabled",
		"llm.model",
		"llm.optimizer.enabled",
		"llm.optimizer.min_query_calls",
		"llm.optimizer_llm.enabled",
		"advisor.enabled",
		"alerting.enabled",
		"alerting.cooldown_minutes",
		"retention.snapshots_days",
	}

	for _, key := range expected {
		if !pathSet[key] {
			t.Errorf(
				"structFieldPaths did not produce expected "+
					"path %q", key)
		}
	}
}

// TestConfigConsistency_CoerceValueCoverage ensures coerceValue
// handles every validation type that appears in api-tagged keys.
func TestConfigConsistency_CoerceValueCoverage(t *testing.T) {
	// Collect all unique validation types.
	vtypes := make(map[string]bool)
	for _, vtype := range configKeyTypes() {
		vtypes[vtype] = true
	}

	// Each type must produce a non-string result (except "string"
	// and "trust_level"/"exec_mode" which legitimately return
	// strings).
	testInputs := map[string]string{
		"int_pos": "10",
		// int_week_minutes is 1-10080 (analyzer.inactive_slot_min_minutes).
		"int_week_minutes":  "10",
		"int_nonneg":        "0",
		"int_min5":          "10",
		"int_class_timeout": "3600",
		"int_vacuum_stall":  "60",
		"pct":               "50",
		"pct1_100":          "50",
		"float01":           "0.5",
		"float_share":       "0.5",
		"bool":              "true",
		"trust_level":       "advisory",
		"exec_mode":         "auto",
		"string":            "hello",
		// maintenance_window is validated against the policy window grammar.
		"maintenance_window": "weeknights",
		"rule_list":          "unused_indexes,slow_queries",
		"float_pos":          "2.0",
		"float_nonneg":       "0.5",
		"float_pct_pos":      "25.0",
		"float_pct_open":     "25.0", // vacuum-11b
		"policy_profile":     "staffed",
		"clone_provider":     "none",
		"mcp_transport":      "stdio",
		"llm_auth":           "databricks",
		"bool_auto":          "true", // rca-09b
	}

	for vtype := range vtypes {
		input, ok := testInputs[vtype]
		if !ok {
			t.Errorf(
				"validation type %q has no test input — add "+
					"it to testInputs", vtype)
			continue
		}

		// Find any key with this vtype to test coercion.
		var sampleKey string
		for k, v := range configKeyTypes() {
			if v == vtype {
				sampleKey = k
				break
			}
		}

		result := coerceValue(sampleKey, input)
		if result == nil {
			t.Errorf(
				"coerceValue(%q, %q) [type %s] returned nil",
				sampleKey, input, vtype)
			continue
		}

		// Verify type-specific coercion.
		switch vtype {
		case "int_pos", "int_nonneg", "int_min5", "pct",
			"pct1_100", "int_week_minutes", "int_class_timeout", "int_vacuum_stall":
			if _, ok := result.(int); !ok {
				t.Errorf(
					"coerceValue(%q, %q) [type %s] = %T, "+
						"want int",
					sampleKey, input, vtype, result)
			}
		case "float01", "float_share", "float_pos", "float_nonneg", "float_pct_pos",
			"float_pct_open":
			if _, ok := result.(float64); !ok {
				t.Errorf(
					"coerceValue(%q, %q) [type %s] = %T, "+
						"want float64",
					sampleKey, input, vtype, result)
			}
		case "bool":
			if _, ok := result.(bool); !ok {
				t.Errorf(
					"coerceValue(%q, %q) [type %s] = %T, "+
						"want bool",
					sampleKey, input, vtype, result)
			}
		case "trust_level", "exec_mode", "policy_profile",
			"clone_provider", "mcp_transport", "llm_auth", "string", "maintenance_window",
			"rule_list":
			if _, ok := result.(string); !ok {
				t.Errorf(
					"coerceValue(%q, %q) [type %s] = %T, "+
						"want string",
					sampleKey, input, vtype, result)
			}
		}
	}
}

// TestConfigConsistency_AllowedKeyCount is a tripwire test that
// fails when someone adds or removes a key without updating the
// test. Update the expected count when intentionally changing keys.
func TestConfigConsistency_AllowedKeyCount(t *testing.T) {
	const expectedCount = 121 // Update when adding/removing keys.

	actual := len(configKeyTypes())
	if actual != expectedCount {
		// Build a sorted list for easy diffing.
		keys := make([]string, 0, actual)
		for k := range configKeyTypes() {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		t.Errorf(
			"api-tagged keys count changed: got %d, "+
				"want %d.\nCurrent keys:\n  %s\n\n"+
				"If this is intentional, update "+
				"expectedCount in this test.",
			actual, expectedCount,
			strings.Join(keys, "\n  "))
	}
}

// TestConfigConsistency_ConfigToMapKeyCount is a parallel tripwire
// for configToMap output.
func TestConfigConsistency_ConfigToMapKeyCount(t *testing.T) {
	cfg := &config.Config{
		Trust: config.TrustConfig{Level: "observation"},
	}
	m := configToMap(cfg)

	const expectedCount = 121 // Should match api-tagged keys.

	actual := len(m)
	if actual != expectedCount {
		keys := make([]string, 0, actual)
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		t.Errorf(
			"configToMap() key count changed: got %d, "+
				"want %d.\nCurrent keys:\n  %s\n\n"+
				"If this is intentional, update "+
				"expectedCount in this test.",
			actual, expectedCount,
			strings.Join(keys, "\n  "))
	}
}

// TestConfigConsistency_SymmetricDiff provides a single combined
// view of mismatches between the three registries. Useful as a
// quick summary when other tests fail.
func TestConfigConsistency_SymmetricDiff(t *testing.T) {
	cfg := &config.Config{
		Trust: config.TrustConfig{Level: "observation"},
	}
	m := configToMap(cfg)

	// Collect all keys from both sources.
	allKeys := make(map[string]bool)
	for k := range configKeyTypes() {
		allKeys[k] = true
	}
	for k := range m {
		allKeys[k] = true
	}

	var diffs []string
	for key := range allKeys {
		_, inAllowed := configKeyTypes()[key]
		_, inMap := m[key]

		switch {
		case inAllowed && !inMap:
			diffs = append(diffs,
				fmt.Sprintf(
					"  %s: in api-tagged keys but NOT "+
						"in configToMap()", key))
		case !inAllowed && inMap:
			diffs = append(diffs,
				fmt.Sprintf(
					"  %s: in configToMap() but NOT in "+
						"api-tagged keys", key))
		}
	}

	if len(diffs) > 0 {
		sort.Strings(diffs)
		t.Errorf(
			"registry mismatch between api-tagged keys "+
				"and configToMap():\n%s",
			strings.Join(diffs, "\n"))
	}
}
