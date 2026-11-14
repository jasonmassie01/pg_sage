package findingcat

// Severity sets the entries share.
var (
	sevI   = []string{"info"}
	sevW   = []string{"warning"}
	sevC   = []string{"critical"}
	sevIW  = []string{"info", "warning"}
	sevWC  = []string{"warning", "critical"}
	sevIWC = []string{"info", "warning", "critical"}
)

// ProducerAnalyzer emits the deterministic Tier-1 rule categories,
// shown under the rules subsystem.
const ProducerAnalyzer = "analyzer"

// a declares one analyzer rule category.
func a(name string, sev, keys, actions []string, summary string) Category {
	return Category{Name: name, Subsystem: "rules", Producer: ProducerAnalyzer,
		Severities: sev, ConfigKeys: keys, Actions: actions, Summary: summary}
}

// k and act build the key and action lists.
func k(keys ...string) []string {
	return keys
}

func act(acts ...string) []string {
	return acts
}

var analyzerEntries = []Category{
	a("index_health", sevIWC, nil, nil,
		"Index health roll-up kept for rows older releases wrote; nothing emits it today."),
	a("unused_index", sevIW, k("analyzer.unused_index_window_days"), act("drop_unused_index"),
		"An index with no scans over the unused-index window."),
	a("invalid_index", sevIWC, nil, act("drop_unused_index", "reindex_concurrently"),
		"An index left invalid by a failed concurrent build."),
	a("duplicate_index", sevIWC, nil, act("drop_unused_index"),
		"An index whose columns duplicate or are a prefix of another index."),
	a("missing_fk_index", sevIW, nil, act("create_index_concurrently"),
		"A foreign key whose referencing columns have no supporting index."),
	a("index_bloat", sevIW,
		k("analyzer.index_bloat_threshold_pct", "analyzer.index_bloat_min_bytes"),
		act("reindex_concurrently"),
		"A btree index whose catalog-statistics bloat estimate reaches the threshold."),
	a("slow_query", sevWC, k("analyzer.slow_query_threshold_ms", "analyzer.slow_query_min_calls"),
		nil, "A statement whose mean time over the interval exceeds the slow-query threshold."),
	a("high_plan_time", sevWC, nil, nil,
		"A statement that spends more time planning than executing."),
	a("query_regression", sevWC,
		k("analyzer.regression_threshold_pct", "analyzer.regression_lookback_days"), nil,
		"A statement whose mean time rose past the regression threshold against its baseline."),
	a("workload_anomaly", sevWC,
		k("baseline.enabled", "baseline.weeks", "baseline.timezone", "baseline.z_warning",
			"baseline.z_critical", "baseline.max_query_keys", "baseline.min_delta_ms"), nil,
		"A workload metric far above its usual value for this hour of the week."),
	a("seq_scan_heavy", sevWC, k("analyzer.seq_scan_min_rows"), nil,
		"A large table read mostly by sequential scans over the interval."),
	a("high_total_time", sevIWC, nil, nil,
		"A statement whose total time exceeds 10% of the interval, even when each call is fast."),
	a("temp_spill", sevW, nil, nil,
		"A statement writing temporary files over the interval: work_mem is too small for it."),
	a("lock_chain", sevIWC,
		k("analyzer.lock_chain.min_blocked_threshold",
			"analyzer.lock_chain.idle_in_tx_terminate_minutes",
			"analyzer.lock_chain.active_query_cancel_minutes"),
		act("cancel_backend", "terminate_backend"),
		"A root blocker and the sessions waiting on it."),
	a("connection_leak", sevWC, k("analyzer.idle_in_transaction_timeout_minutes"),
		act("terminate_backend"), "A session idle in transaction past the leak timeout."),
	a("cache_hit_ratio", sevWC, k("analyzer.cache_hit_ratio_warning"), nil,
		"The database's buffer cache hit ratio below the warning level."),
	a("checkpoint_pressure", sevWC, k("analyzer.checkpoint_frequency_warning_per_hour"), nil,
		"Requested checkpoints more often than the hourly warning level."),
	a("stat_statements_pressure", sevWC, nil, nil,
		"pg_stat_statements above 80% of pg_stat_statements.max, where entries are evicted."),
	a("replication_lag", sevWC, nil, nil, "A standby replaying WAL behind the primary."),
	a("inactive_slot", sevWC, k("analyzer.inactive_slot_min_minutes"), nil,
		"A replication slot inactive past the threshold, retaining WAL."),
	a("slow_replication_slot", sevWC, k("analyzer.slow_slot_retained_bytes"), nil,
		"An active replication slot retaining at least 1 GB of WAL: a slow consumer."),
	a("subscription_errors", sevWC, nil, nil,
		"A subscription whose apply or sync workers keep failing, or that is disabled "+
			"after errors."),
	a("subscription_lag", sevW, k("analyzer.subscription_lag_minutes"), nil,
		"An enabled subscription that has not reported to its publisher recently, "+
			"or has no apply worker."),
	a("cdc_consumer_stalled", sevW, k("analyzer.cdc_stall_minutes"), nil,
		"An active logical slot whose confirmed_flush_lsn stood still while WAL "+
			"advanced."),
	a("cdc_heartbeat_stale", sevW, k("replication.cdc_heartbeats"), nil,
		"A configured CDC heartbeat table not written within its max age."),
	a("wal_archive_failing", sevWC, nil, nil,
		"The WAL archiver failing since its last successful archive (critical after 30 min)."),
	a("wal_archive_stale", sevWC, k("backup.max_archive_gap_minutes"), nil,
		"Completed WAL waiting longer than the archive gap since the last archive."),
	a("pg_cron_job_failed", sevIW, nil, nil,
		"A pg_cron job whose latest run in the last 24 hours failed (info: the "+
			"monitoring role sees only its own jobs)."),
	a("pg_cron_history_unbounded", sevI, nil, nil,
		"pg_cron's cron.job_run_details past an estimated 1,000,000 rows; pg_cron "+
			"never purges it."),
	a("partman_premake_gap", sevW, nil, nil,
		"A pg_partman parent with fewer future child partitions than its premake."),
	a("partman_default_rows", sevW, nil, nil,
		"Rows in the default partition of a pg_partman parent."),
	a("timescale_job_failed", sevW, nil, nil,
		"A scheduled TimescaleDB job (compression, retention, refresh or a "+
			"user action) whose last run failed."),
	a("timescale_cagg_stale", sevW, nil, nil,
		"A TimescaleDB continuous aggregate whose watermark lags more than 3 x its "+
			"refresh policy's schedule_interval plus end_offset, while the policy has "+
			"not succeeded within that time."),
	a("sequence_exhaustion", sevWC, nil, act("prepare_sequence_capacity_migration"),
		"A sequence that has used most of its range."),
	a("sort_without_index", sevW, nil, nil,
		"A Sort feeding a Limit that sorts at least 10x the rows returned."),
	a("work_mem_promotion", sevI, k("analyzer.work_mem_promotion_threshold"),
		act("promote_role_work_mem"), "A role with work_mem hints on five or more queries."),
	a("table_bloat", sevIWC,
		k("analyzer.table_bloat_dead_tuple_pct", "analyzer.table_bloat_min_rows",
			"analyzer.table_bloat_min_bytes"), act("vacuum_table"),
		"A table whose dead tuples exceed the bloat threshold."),
	a("xid_wraparound", sevIWC,
		k("analyzer.xid_wraparound_warning", "analyzer.xid_wraparound_critical"), nil,
		"The database's transaction ID age approaching wraparound."),
	a("extension_drift", sevW, nil, nil,
		"An installed extension whose version lags its default version."),
	a("plan_regression", sevIWC, nil, nil,
		"A statement whose plan changed and whose estimated cost rose."),
	a("wraparound_freeze", sevWC, k("analyzer.wraparound_freeze_share"), act("vacuum_table"),
		"A leaf table whose freeze age is near autovacuum_freeze_max_age."),
	a("autovacuum_tuning", sevW, k("analyzer.autovacuum_tune_min_rows"),
		act("set_table_autovacuum"),
		"A large table whose autovacuum settings let dead tuples pile up."),
	a("sage_footprint", sevW, nil, nil,
		"pg_sage's own statements using a large share of the database's time."),
	a("sage_self_cost", sevW, k("analyzer.self_cost_budget_ms"), nil,
		"pg_sage's own collection cost above its per-cycle budget."),
	a("sage_self_budget", sevW, nil, nil,
		"A pg_sage self-budget (probe time, model tokens) exhausted for the period."),
	a("clone_schemas", sevI, nil, nil,
		"A family of schemas cloned from one template, reported once instead of per schema."),
	a("app_managed_index", sevI, nil, nil,
		"An index an application or migration tool manages: pg_sage will not drop it."),
	{Name: "stale_statistics", Subsystem: "rules", AlsoIn: []string{"query_tuning"},
		Producer: ProducerAnalyzer, Severities: sevIW,
		ConfigKeys: k("analyzer.analyze_stale_min_rows", "analyzer.analyze_stale_days"),
		Actions:    act("analyze_table"),
		Summary:    "A table changed past its autoanalyze threshold since its last analyze."},
}
