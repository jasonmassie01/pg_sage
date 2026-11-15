package config

type BriefingConfig struct {
	Schedule        string   `yaml:"schedule"`
	Channels        []string `yaml:"channels"`
	SlackWebhookURL string   `yaml:"slack_webhook_url"`
	FleetMode       string   `yaml:"fleet_mode" doc:"Fleet and meta-db modes: rollup (default) posts one briefing message covering every database; per_database posts one per database. Each database still stores its own briefing. Restart to change."`
}

type AlertingConfig struct {
	Enabled              bool            `yaml:"enabled" api:"bool"`
	Engine               string          `yaml:"engine" doc:"Engine for the alerting block: notify (reconciled into notification channels and rules, sent through the outbox) or legacy (old per-database manager). Restart. Default: notify."`
	CheckIntervalSeconds int             `yaml:"check_interval_seconds" api:"int_min5"`
	CooldownMinutes      int             `yaml:"cooldown_minutes" api:"int_nonneg"`
	RepeatMinutes        map[string]int  `yaml:"repeat_minutes" doc:"Minutes between reminders of a finding that stays open, per severity. A missing key takes its default; 0 never reminds. Default: critical 240, warning 1440, info 0."`
	QuietHoursStart      string          `yaml:"quiet_hours_start" api:"string"`
	QuietHoursEnd        string          `yaml:"quiet_hours_end" api:"string"`
	Timezone             string          `yaml:"timezone"`
	SlackWebhookURL      string          `yaml:"slack_webhook_url" api:"string"`
	PagerDutyRoutingKey  string          `yaml:"pagerduty_routing_key" api:"string"`
	Routes               []AlertRoute    `yaml:"routes"`
	Webhooks             []WebhookConfig `yaml:"webhooks"`
}

type AlertRoute struct {
	Severity string   `yaml:"severity"`
	Channels []string `yaml:"channels"`
}

type WebhookConfig struct {
	Name    string            `yaml:"name"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
}

type AutoExplainConfig struct {
	Enabled                bool `yaml:"enabled"`
	LogMinDurationMs       int  `yaml:"log_min_duration_ms"`
	CollectIntervalSeconds int  `yaml:"collect_interval_seconds"`
	MaxPlansPerCycle       int  `yaml:"max_plans_per_cycle"`
}

type ForecasterConfig struct {
	Enabled                    bool    `yaml:"enabled"`
	LookbackDays               int     `yaml:"lookback_days"`
	DiskWarnGrowthGBDay        float64 `yaml:"disk_warn_growth_gb_day"`
	ConnectionWarnPct          float64 `yaml:"connection_warn_pct"`
	CacheWarnThreshold         float64 `yaml:"cache_warn_threshold"`
	CacheMinBlocksPerDay       int64   `yaml:"cache_min_blocks_per_day" doc:"Block accesses (blks_hit + blks_read over a closed day) below which the day's cache hit ratio is too thin to judge; forecast_cache_pressure leaves such a day out. At least 1. Default: 100000."`
	CheckpointRequestedPerHour float64 `yaml:"checkpoint_requested_per_hour" doc:"Requested (WAL-triggered or explicit) checkpoints per hour, as an EWMA over closed days, at which forecast_checkpoint_pressure warns. Timed checkpoints are not counted. 0 < value <= 3600. Default: 2."`

	DiskCapacityBytes int64   `yaml:"disk_capacity_bytes" doc:"Disk capacity in bytes for the disk runway, disk-full credit and days_until_full. 0 = undeclared: never auto-detected self-hosted. Managed: a fresh provider capacity wins; never credited."`
	MinRSquared       float64 `yaml:"min_r_squared" doc:"Minimum R-squared a disk growth or connection saturation trend needs before it is projected; below it there is no finding. 0 means the default. Default: 0.5."`
}

// RCAConfig controls the root cause analysis engine (v0.9).
type RCAConfig struct {
	Enabled                    bool    `yaml:"enabled" doc:"Enable the root cause analysis engine. Correlates multiple signals into unified incident narratives." api:"bool"`
	LLMCorrelationThreshold    int     `yaml:"llm_correlation_threshold" doc:"Minimum concurrent signals to trigger LLM-based correlation (Tier 2). Default: 3." api:"int_pos"`
	DedupWindowMinutes         int     `yaml:"dedup_window_minutes" doc:"Time window for deduplicating incidents with the same signals and root object. Default: 30." api:"int_pos" range:"1,525600"`
	EscalationCycles           int     `yaml:"escalation_cycles" doc:"Consecutive analyzer cycles an incident stays open before escalating warning to critical. Default: 5." api:"int_pos"`
	ResolutionCycles           int     `yaml:"resolution_cycles" doc:"Consecutive clear cycles before an incident auto-resolves. Default: 2." api:"int_pos"`
	StaleAfterHours            int     `yaml:"stale_after_hours" doc:"Hours an open incident may go unseen before pg_sage resolves it as stale (no notification when it was last seen that long ago). 1-8760, at least the dedup window. Default: 24."`
	VacuumMinDeadTuples        int     `yaml:"vacuum_min_dead_tuples" doc:"Fewest dead tuples a table needs before its dead-tuple ratio counts toward the vacuum_blocked (autovacuum falling behind) incident. 1-1000000000. Default: 1000."`
	VacuumMinTableMB           int     `yaml:"vacuum_min_table_mb" doc:"Smallest heap, in MB, a table needs before its dead-tuple ratio counts toward the vacuum_blocked incident. 1-1048576. Default: 8."`
	XminHolderMinMinutes       int     `yaml:"xmin_holder_min_minutes" doc:"Minutes a session's transaction must be open before the vacuum_blocked incident names it as the holder of the oldest xmin. 1-1440. Default: 5."`
	PreparedXactMinAgeMinutes  int     `yaml:"prepared_xact_min_age_minutes" doc:"Minutes a prepared transaction of this database must be old before it opens orphaned_prepared_tx or is named the vacuum_blocked holder. 1-10080. Default: 30."`
	ConnectionSaturationPct    int     `yaml:"connection_saturation_pct" doc:"Percentage of max_connections that triggers the connections_high signal. Default: 80." api:"pct"`
	ReplicationLagThresholdS   int     `yaml:"replication_lag_threshold_seconds" doc:"Seconds of replay lag before the replication_lag_increasing signal fires. Default: 30." api:"int_pos"`
	WALSpikeMultiplier         float64 `yaml:"wal_spike_multiplier" doc:"wal_growth_spike fires when the WAL rate (insert position advance per minute) of a collector interval is this many times the previous interval's and at least 1 MiB/min. Default: 2.0." api:"float_pos"`
	LockChainIntervalSeconds   int     `yaml:"lock_chain_interval_seconds" doc:"Seconds between lock-chain fast-path checks, which open or update the lock_contention incident between analyzer cycles. 0 disables the fast path; otherwise 10-3600. Default: 60."`
	NarrationEnabled           bool    `yaml:"narration_enabled" doc:"Add an LLM narrative, citing the incident's evidence and catalog probe results, to incident notifications. Off, no LLM, or any LLM failure uses the deterministic summary. Default: true." api:"bool"`
	LogFastPathIntervalSeconds int     `yaml:"log_fast_path_interval_seconds" doc:"Seconds between log fast-path passes, which open incidents for critical log lines (PANIC, disk full, OOM, wraparound) between analyzer cycles. 0 disables; otherwise 1-3600. Default: 5."`
	LogQuietMinutes            int     `yaml:"log_quiet_minutes" doc:"Minutes without a new log line before a log incident auto-resolves (not clear cycles). 0 = rca.dedup_window_minutes; else 1-1440, within stale_after_hours. Default: 0."`
	RecurrenceRenotifyHours    int     `yaml:"recurrence_renotify_hours" doc:"Hours after a log incident resolves in which a recurrence at no higher severity is linked but not notified again. 1-168. Default: 6."`
	DeadlockStatementText      string  `yaml:"deadlock_statement_text" doc:"Deadlock participant statements: fingerprint (hash of the literal-redacted statement, no text) or redacted (also its redacted text, experimental). Default: fingerprint."`
}

// LockChainConfig controls lock chain detection (v0.9).
type LockChainConfig struct {
	Enabled                  bool     `yaml:"enabled" doc:"Enable lock chain detection using pg_blocking_pids(). Detects cascading lock waits." api:"bool"`
	MinBlockedThreshold      int      `yaml:"min_blocked_threshold" doc:"Minimum blocked sessions before a lock chain generates a finding. Default: 3." api:"int_pos"`
	CriticalBlockedThreshold int      `yaml:"critical_blocked_threshold" doc:"Blocked session count that escalates finding to critical severity. Default: 10." api:"int_pos"`
	IdleInTxTerminateMinutes int      `yaml:"idle_in_tx_terminate_minutes" doc:"Minutes an idle-in-transaction root blocker must wait before pg_terminate_backend is recommended. Default: 5." api:"int_pos"`
	ActiveQueryCancelMinutes int      `yaml:"active_query_cancel_minutes" doc:"Minutes an active query root blocker must run before pg_cancel_backend is recommended. Default: 15." api:"int_pos"`
	SafePatterns             []string `yaml:"safe_patterns" doc:"Application name substrings that protect a process from automated termination/cancellation."`
}

// LogWatchConfig controls PostgreSQL log file monitoring (v0.9.1).
type LogWatchConfig struct {
	Enabled             *bool    `yaml:"enabled" doc:"Log RCA from local jsonlog/csvlog files or a provider log API: true, false, or auto (unset, the default: on once a readable source is detected). Needs rca.enabled." api:"bool_auto"`
	LogDirectory        string   `yaml:"log_directory" doc:"Path to PG log directory. Auto-detected from pg_settings if blank." api:"string"`
	Format              string   `yaml:"format" doc:"Log format: jsonlog (PG15+) or csvlog. Auto-detected if blank." api:"string"`
	PollIntervalMs      int      `yaml:"poll_interval_ms" doc:"Log file poll interval (the tailer only polls). Default: 1000." api:"int_pos"`
	DedupWindowS        int      `yaml:"dedup_window_seconds" doc:"Suppress duplicate log signals within this window. Default: 60." api:"int_pos"`
	MaxLineLenBytes     int      `yaml:"max_line_len_bytes" doc:"Drop log records longer than this (an auto_explain plan record may reach 1 MiB); drops are counted as oversized_record. Default: 65536." api:"int_pos"`
	TempFileMinBytes    int      `yaml:"temp_file_min_bytes" doc:"Minimum temp file size to trigger log_temp_file_created. Default: 10485760." api:"int_pos"`
	MaxLinesPerCycle    int      `yaml:"max_lines_per_cycle" doc:"Stop parsing after this many lines per drain cycle. Default: 10000." api:"int_pos"`
	ExcludeApplications []string `yaml:"exclude_applications" doc:"Application names excluded from timeout/deadlock signals. Default: pg_sage." api:"string"`
	SlowQueryEnabled    bool     `yaml:"slow_query_enabled" doc:"Opt-in: parse log_min_duration_statement output as log_slow_query signals. Default: false." api:"bool"`
}

// RunawayPolicy defines when a query is considered runaway (v0.9).
type RunawayPolicy struct {
	Name               string `yaml:"name"`
	MaxDurationMinutes int    `yaml:"max_duration_minutes"`
	MaxBlockedSessions int    `yaml:"max_blocked_sessions"`
	WarnCycles         int    `yaml:"warn_cycles"`
	CancelCycles       int    `yaml:"cancel_cycles"`
}

// RunawayConfig controls runaway query termination (v0.9).
type RunawayConfig struct {
	Enabled      bool            `yaml:"enabled" doc:"Enable policy-based runaway query detection and escalation (warn -> cancel -> terminate)." api:"bool"`
	Policies     []RunawayPolicy `yaml:"policies" doc:"List of runaway query policies. A query matching any policy enters the escalation state machine."`
	SafePatterns []string        `yaml:"safe_patterns" doc:"Application name substrings protected from runaway termination. Merged with lock_chain.safe_patterns at startup."`
}

// ExplainConfig controls the natural language EXPLAIN feature (v0.9).
type ExplainConfig struct {
	Enabled         bool `yaml:"enabled" doc:"Enable the LLM-powered natural language EXPLAIN API endpoint." api:"bool"`
	TimeoutMs       int  `yaml:"timeout_ms" doc:"statement_timeout for EXPLAIN ANALYZE execution. Default: 10000." api:"int_pos"`
	CacheTTLMinutes int  `yaml:"cache_ttl_minutes" doc:"TTL for cached explain results. Default: 60." api:"int_pos"`
	MaxTokens       int  `yaml:"max_tokens" doc:"Maximum LLM output tokens for explain responses. Default: 4096." api:"int_pos"`
}

// SchemaLintConfig controls schema anti-pattern detection (v0.10).
type SchemaLintConfig struct {
	Enabled             bool     `yaml:"enabled" doc:"Enable periodic schema anti-pattern scanning." api:"bool"`
	ScanIntervalMinutes int      `yaml:"scan_interval_minutes" doc:"How often to run the full schema lint scan. Default: 60." api:"int_pos"`
	IncludeSchemas      []string `yaml:"include_schemas" doc:"Schemas to scan. Empty means all non-system schemas."`
	ExcludeSchemas      []string `yaml:"exclude_schemas" doc:"Schemas to exclude. Default: pg_catalog, information_schema."`
	DisabledRules       []string `yaml:"disabled_rules" doc:"Rule IDs to skip (e.g. lint_serial_usage)."`
	MinTableRows        int      `yaml:"min_table_rows" doc:"Ignore findings on tables below this row count. Default: 1000." api:"int_pos" zero:"default"`
}

// MigrationConfig controls the DDL safety advisor (v0.10).
type MigrationConfig struct {
	Enabled               bool   `yaml:"enabled" doc:"Enable DDL safety advisory." api:"bool"`
	Mode                  string `yaml:"mode" doc:"Operating mode: advisory (default). Active mode deferred to future release." api:"string"`
	LogDetection          bool   `yaml:"log_detection" doc:"Parse DDL from PG logs (requires log_statement=ddl)." api:"bool"`
	ActivityPolling       bool   `yaml:"activity_polling" doc:"Scan pg_stat_activity for active DDL." api:"bool"`
	PollIntervalSeconds   int    `yaml:"poll_interval_seconds" doc:"How often to poll during active DDL. Default: 5." api:"int_pos"`
	DDLRowThreshold       int    `yaml:"ddl_row_threshold" doc:"Row count below which DDL is considered low-risk. Default: 10000." api:"int_pos" zero:"default"`
	QueueWaitersThreshold int    `yaml:"queue_waiters_threshold" doc:"Sessions queued directly behind an in-flight DDL waiting for its lock before the finding turns critical with approval-only cancel guidance. Needs the RCA fast path. Default: 3." api:"int_pos" zero:"default"`
	// migration-06: rehearsed apply_migration budgets (migration_rehearsal.go).
	ContractDelayMinutes int `yaml:"contract_delay_minutes" doc:"Minutes after apply_migration's expand phase before its contract phase may run; recorded as contract_not_before. 1 or more. Default: 60."`
	MaxLockMS            int `yaml:"max_lock_ms" doc:"Milliseconds a rehearsed step without CONCURRENTLY may hold ACCESS EXCLUSIVE on the clone before apply_migration parks (step_budget_exceeded). 1 or more. Default: 2000."`
}

type TunerConfig struct {
	Enabled                bool    `yaml:"enabled" doc:"Master switch for the per-query tuner. When false, no hints are written and Tune() is a no-op — the analyzer still runs."`
	LLMEnabled             bool    `yaml:"llm_enabled" doc:"Let the tuning agent propose per-query pg_hint_plan hints for its cases; the tuner validates, clamps and records them. The tuner's deterministic hint rules run either way. Default: true."`
	WorkMemMaxMB           int     `yaml:"work_mem_max_mb" doc:"Maximum per-query work_mem (MB) the tuner is allowed to prescribe via Set(work_mem) hints. Ceiling prevents runaway memory suggestions."`
	PlanTimeRatio          float64 `yaml:"plan_time_ratio" doc:"When plan_time / exec_time exceeds this ratio, the tuner flags the query as plan-time-dominated and reports query_parameterization advice (prepared statements); it prescribes no hint."`
	NestedLoopRowThreshold int64   `yaml:"nested_loop_row_threshold" doc:"Total rows (Actual Rows x Actual Loops) a misestimated (>10x) nested loop's inner side must read before the tuner prescribes HashJoin or ANALYZE. 0 or less means 10000."`
	ParallelMinTableRows   int64   `yaml:"parallel_min_table_rows" doc:"Threshold for suggesting parallel-enable hints on large sequential scans. Tables smaller than this are assumed not to benefit from parallelism."`
	MinQueryCalls          int     `yaml:"min_query_calls" doc:"Require at least this many pg_stat_statements.calls before the tuner will act on a query. Filters out one-offs."`
	VerifyAfterApply       bool    `yaml:"verify_after_apply" doc:"Run the daily hint revalidation loop: broken when a named index is gone, dormant while the query is idle, re-baselined after a stats reset. Never removes a working hint. Requires pg_hint_plan." warning:"v0.8.5 activates a real background cycle here — upgrade note: if you want v0.8.4 inert behavior, set this to false before upgrading."`

	// v0.8.5 Feature 1 — Hint revalidation loop.
	HintRetirementDays        int `yaml:"hint_retirement_days" doc:"Hints older than this many days are stamped due for re-verification (reverify_due); they stay installed. 0 disables the age rule. Default 14."`
	RevalidationIntervalHours int `yaml:"revalidation_interval_hours" doc:"How often the hint revalidation loop runs. Set to 0 to disable the loop entirely. Default 24 hours."`
	// tuning-08: cached plans have an age limit.
	PlanMaxAgeMinutes int `yaml:"plan_max_age_minutes" doc:"A cached plan this many minutes old is stale, as is one older than its tables' last ANALYZE or a pg_sage action on them; stale plans never drive hints. 0 or less means 1440 (default)."`
	// tuning-11: parameterization advice for IN-list shapes.
	ParamShapeMinVariants int `yaml:"param_shape_min_variants" doc:"Queryids of one statement shape, its IN and ANY(ARRAY[...]) parameter lists collapsed, before a query_parameterization finding. Off on PG18 (lists squashed). 2-1000. Default 20."`
	// tuning-05: hinted EXPLAIN gate.
	HintCostTolerancePct float64 `yaml:"hint_cost_tolerance_pct" doc:"Percent a join or scan hint may raise the estimated plan cost in the hinted EXPLAIN before it is refused (cost_regression); an unchanged plan is refused (no_effect). 0-100. Default 10."`
	// tuning-06: open query_tuning findings resolve.
	HintAbsentCycles int `yaml:"hint_absent_cycles" doc:"Analyzer cycles a query_tuning finding's statement must be absent from pg_stat_statements, or below the tuner's admission thresholds, before it resolves. 0 or less means 3. Default 3."`

	// v0.8.5 Feature 2 — Stale-stats detection + ANALYZE action.
	StaleStatsEstimateSkew float64 `yaml:"stale_stats_estimate_skew" doc:"Ratio ActualRows / PlanRows above which a plan node is considered row-estimate skewed. Default 10 — same threshold as the bad-nested-loop check."`
	StaleStatsModRatio     float64 `yaml:"stale_stats_mod_ratio" doc:"Fraction of rows modified since last ANALYZE (n_mod_since_analyze / reltuples) required to treat a table as stale. Default 0.1 = 10%."`
	StaleStatsAgeMinutes   int     `yaml:"stale_stats_age_minutes" doc:"Both manual and autovacuum last_analyze must be older than this many minutes before the table is considered stale. Default 60."`
	AnalyzeMaxTableMB      int64   `yaml:"analyze_max_table_mb" doc:"Tables larger than this many MB are never auto-ANALYZEd — an advisory finding is emitted instead for operator review." warning:"Default 10240 (10GB). Raise with care — ANALYZE on TB-scale tables can saturate I/O."`
	AnalyzeCooldownMinutes int     `yaml:"analyze_cooldown_minutes" doc:"Minimum minutes between tuner-initiated ANALYZE runs on the same table. Also respects autovacuum last_analyze as a recent-run signal."`
	AnalyzeTimeoutMs       int     `yaml:"analyze_timeout_ms" doc:"statement_timeout for ANALYZE actions. Default 600000 (10 minutes). Exceeding this marks the action failed_timeout and extends the cooldown."`
	MaxConcurrentAnalyze   int     `yaml:"max_concurrent_analyze" doc:"Hard cap on concurrent ANALYZE actions across the entire sidecar process (shared semaphore — fleet-wide, not per-database). Default 1."`
}

type RetentionConfig struct {
	SnapshotsDays int `yaml:"snapshots_days" api:"int_pos"`
	FindingsDays  int `yaml:"findings_days" api:"int_pos"`
	ActionsDays   int `yaml:"actions_days" api:"int_pos"`
	ExplainsDays  int `yaml:"explains_days" api:"int_pos"`
	DecisionsDays int `yaml:"decisions_days" doc:"Days to keep parked, queued, blocked and observe-only decisions after they were last seen; ones behind an action or verification are kept. 0 disables. Range 0-3650. Default 30."`
	AuthAuditDays int `yaml:"auth_audit_days" doc:"Days to keep the sign-in audit trail (sage.auth_audit: SSO logins, links, grants and break-glass use). 0 keeps it forever. Range 0-3650. Default 365."`
	// SageSizeWarningPct: see sage_footprint.go.
	SageSizeWarningPct int `yaml:"sage_size_warning_pct" doc:"Raise a sage_footprint finding when pg_sage's own tables (the sage schema) exceed this percent of the database size; below 256 MB it is never a finding. 0 disables the check. Default 10." api:"pct"`
	// QueryStoreDays and SnapshotsMaxPct: see storage_retention.go.
	QueryStoreDays  int `yaml:"query_store_days" doc:"Days to keep per-query samples in sage.query_store (its readers look back at most 7 days). 0 disables. Range 0-3650. Default 14."`
	SnapshotsMaxPct int `yaml:"snapshots_max_pct" doc:"Cap sage.snapshots at this percent of the database size (never below 256 MB): the oldest days are removed first. 0 disables the cap. Range 0-100. Default 5."`
	// QueryRollupHourlyDays and QueryRollupDailyDays: see storage_retention.go.
	QueryRollupHourlyDays int `yaml:"query_rollup_hourly_days" doc:"Days to keep the hourly per-query rollup; an hour goes once its day is in the daily tier. At most snapshots_days (the default follows a smaller one). 0 disables. Range 0-3650. Default 35."`
	QueryRollupDailyDays  int `yaml:"query_rollup_daily_days" doc:"Days to keep the daily per-query rollup and the hourly coverage rows. At least query_rollup_hourly_days. 0 disables. Range 0-3650. Default 400."`
	// WorkloadPacksDays: see storage_retention.go.
	WorkloadPacksDays int `yaml:"workload_packs_days" doc:"Days to keep stored workload evidence packs (sage.workload_packs), from when each was built. 0 disables. Range 0-3650. Default 180."`
}

// NotificationPolicyConfig holds notification delivery policy. It is YAML-only
// and restart-bound: the target policy is a security boundary baked into
// senders at startup, so it is deliberately not an API override.
type NotificationPolicyConfig struct {
	AllowPrivateTargets bool `yaml:"allow_private_targets" doc:"Allow webhook and SMTP channels to target loopback, RFC 1918, ULA or CGNAT hosts and plain http (e.g. an internal SMTP relay). Metadata and link-local stay blocked. Requires restart."`
}

// OAuthConfig holds OAuth2/OIDC SSO settings.
type OAuthConfig struct {
	Enabled      bool   `yaml:"enabled" doc:"Enable OAuth single sign-on for the sidecar UI. When false, the UI falls back to local auth."`
	Provider     string `yaml:"provider" doc:"OAuth provider identifier. Supported values include google, github, okta, and generic OIDC."`
	ClientID     string `yaml:"client_id" doc:"OAuth client ID issued by the provider when the pg_sage application was registered."`
	ClientSecret string `yaml:"client_secret" doc:"OAuth client secret issued by the provider. Source from an environment variable in production." secret:"true"`
	RedirectURL  string `yaml:"redirect_url" doc:"Absolute URL the provider redirects to after authentication. Must match the URI registered at the provider."`
	IssuerURL    string `yaml:"issuer_url" doc:"OIDC issuer URL used to discover the provider's authorization and token endpoints."`
	DefaultRole  string `yaml:"default_role" doc:"Role assigned to newly authenticated users when no role mapping rule matches."`
	// Group to role mapping and break-glass (E1); see authz.go.
	GroupsClaim   string             `yaml:"groups_claim" doc:"ID-token (or userinfo) claim that lists the user's IdP groups for role_mapping. Default groups."`
	RoleMapping   []OAuthRoleMapping `yaml:"role_mapping" doc:"Maps IdP groups to pg_sage roles; the highest matching role wins and is re-applied at every SSO login."`
	UnmappedUsers string             `yaml:"unmapped_users" doc:"When role_mapping is set, what a user in no mapped group gets: deny (default, refused) or default_role."`
	BreakGlass    BreakGlassConfig   `yaml:"break_glass"`
	// IdP group to per-database grant mapping (apiauth-08d); see idp_grants.go.
	DatabaseGrantMapping []OAuthDatabaseGrant `yaml:"database_grant_mapping" doc:"Maps IdP groups to per-database grants (viewer or operator), re-applied at every SSO login. Empty (default) leaves grants to admins."`
}
