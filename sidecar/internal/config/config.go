package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// braceEnvRe matches ${NAME} references only (NAME starts with a letter
// or underscore). Bare $VAR is deliberately NOT matched.
var braceEnvRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandBracedEnv expands only ${NAME} references (set → value, unset →
// empty string, matching the documented "empty when unset" behavior). A
// NAME_FILE variable supplies the value from a file (GR-11).
// Unlike os.ExpandEnv it leaves bare '$' untouched, so a literal '$' in
// a password or API key written directly into YAML is no longer
// silently truncated (H5). Used by both initial load and hot-reload.
func expandBracedEnv(raw string) string {
	return braceEnvRe.ReplaceAllStringFunc(raw, func(match string) string {
		name := braceEnvRe.FindStringSubmatch(match)[1]
		return bracedEnvValue(name)
	})
}

// hotReloadMu serializes hot-reload writes and any reads that need a
// self-consistent view of multiple fields. Kept at package scope (not
// as a Config method or field) so Config remains copy-safe for watcher
// snapshots and executor per-cycle overrides — adding Lock/Unlock
// methods would make Config look like sync.Locker and trigger
// "assignment copies lock value" from go vet.
var hotReloadMu sync.RWMutex

// LockForHotReload acquires the package-level config write lock. Call
// from any code path that mutates a live Config struct concurrently
// with background readers.
func LockForHotReload() { hotReloadMu.Lock() }

// UnlockForHotReload releases the package-level config write lock.
func UnlockForHotReload() { hotReloadMu.Unlock() }

// RLockForHotReload acquires the package-level config read lock.
func RLockForHotReload() { hotReloadMu.RLock() }

// RUnlockForHotReload releases the package-level config read lock.
func RUnlockForHotReload() { hotReloadMu.RUnlock() }

// Config is the top-level configuration for pg_sage standalone sidecar.
//
// Hot-reload mutations (from the /api/v1/config API) must be serialized
// against other mutations and against background-loop reads. Mutations
// call cfg.Lock()/cfg.Unlock(); readers that require a self-consistent
// view of multi-field state should use cfg.RLock()/cfg.RUnlock().
// Single-field reads of naturally-aligned scalars may skip locking on
// most architectures but will still trip the race detector — new code
// should prefer the explicit lock.
type Config struct {
	Mode string `yaml:"mode" doc:"Operating mode: standalone, fleet, or meta. Standalone connects to one PostgreSQL target; fleet manages many databases from one sidecar."`

	Postgres    PostgresConfig      `yaml:"postgres"`
	Collector   CollectorConfig     `yaml:"collector"`
	Analyzer    AnalyzerConfig      `yaml:"analyzer"`
	Safety      SafetyConfig        `yaml:"safety"`
	Trust       TrustConfig         `yaml:"trust"`
	LLM         LLMConfig           `yaml:"llm"`
	Advisor     AdvisorConfig       `yaml:"advisor"`
	Briefing    BriefingConfig      `yaml:"briefing"`
	Alerting    AlertingConfig      `yaml:"alerting"`
	AutoExplain AutoExplainConfig   `yaml:"auto_explain"`
	Forecaster  ForecasterConfig    `yaml:"forecaster"`
	Tuner       TunerConfig         `yaml:"tuner"`
	Tuning      TuningConfig        `yaml:"tuning"`
	RCA         RCAConfig           `yaml:"rca"`
	SRE         SREConfig           `yaml:"sre"`
	Runaway     RunawayConfig       `yaml:"runaway"`
	Explain     ExplainConfig       `yaml:"explain"`
	LogWatch    LogWatchConfig      `yaml:"logwatch"`
	SchemaLint  SchemaLintConfig    `yaml:"schema_lint"`
	Migration   MigrationConfig     `yaml:"migration"`
	Rehearsal   RehearsalConfig     `yaml:"rehearsal"`
	Retention   RetentionConfig     `yaml:"retention"`
	Prometheus  PrometheusConfig    `yaml:"prometheus"`
	Azure       AzureConfig         `yaml:"azure"`
	OAuth       OAuthConfig         `yaml:"oauth"`
	Policy      PolicyConfig        `yaml:"policy"`
	Value       ValueConfig         `yaml:"value"`
	Verify      VerifyConfig        `yaml:"verify"`
	Clone       CloneProviderConfig `yaml:"clone"`
	Custodian   CustodianConfig     `yaml:"custodian"`
	MCP         MCPConfig           `yaml:"mcp"`
	Ask         AskConfig           `yaml:"ask"`
	Facts       FactsConfig         `yaml:"facts"`
	SelfBudget  SelfBudgetConfig    `yaml:"self_budget"`
	Baseline    BaselineConfig      `yaml:"baseline"`
	History     HistoryConfig       `yaml:"history"`
	Debug       DebugConfig         `yaml:"debug"`
	SelfConfig  SelfConfigConfig    `yaml:"self_config"`
	// Specialist is the Postgres-specialist contract other agents call.
	Specialist SpecialistConfig `yaml:"specialist"`
	// Budgets are the workload budgets (workload_budgets.go, market-08).
	Budgets []BudgetConfig `yaml:"budgets" doc:"Workload budgets per role, application_name or sqlcommenter tag: warn mode (default) counts would-be violations; enforce (role budgets) proposes ALTER ROLE settings. Default: none."`
	// FleetLearning is fleet learning and leader election (fleet_learning.go).
	FleetLearning FleetLearningConfig `yaml:"fleet_learning"`
	// Fleet holds fleet auto-discovery (fleet_discovery.go, market-06).
	Fleet FleetConfig `yaml:"fleet"`
	// HA is each database's HA monitor cadence (ha.go).
	HA HAConfig `yaml:"ha"`
	// Backup is the backup freshness policy (backup.go, market-07).
	Backup BackupConfig `yaml:"backup"`
	// CloudTelemetry is managed-cloud host telemetry (cloud_telemetry.go).
	CloudTelemetry        CloudTelemetryConfig        `yaml:"cloud_telemetry"`
	LakebaseObservability LakebaseObservabilityConfig `yaml:"lakebase_observability"`
	// ProviderAdvice is provider advisor recommendations (provider_advice.go).
	ProviderAdvice ProviderAdviceConfig `yaml:"provider_advice"`
	// Cost is operator-supplied pricing (cost.go, market-04).
	Cost CostConfig `yaml:"cost"`
	// Agents is agent posture (agents.go).
	Agents AgentsConfig `yaml:"agents"`
	// Audit is the audit export and evidence of E2 (audit.go).
	Audit AuditConfig `yaml:"audit"`
	// Replication is CDC consumer health (replication.go, market-13).
	Replication ReplicationConfig `yaml:"replication"`

	// NotificationPolicy governs notification channel targets (G7-B21). The
	// top-level "notifications" key is retired (see rejectRetiredTopLevelConfig).
	NotificationPolicy NotificationPolicyConfig `yaml:"notification_policy"`

	// Checks are owner-defined read-only checks (checks.go, market-11).
	Checks []CustomCheck `yaml:"checks" doc:"Owner-defined read-only checks: one SELECT each, run read-only; each row is a custom:<id> finding (docs/custom-checks.md). Default: empty."`

	// Fleet mode fields.
	Databases []DatabaseConfig `yaml:"databases"`
	Defaults  DefaultsConfig   `yaml:"defaults"`
	API       APIConfig        `yaml:"api"`

	// Meta database and encryption (--meta-db, --encryption-key).
	MetaDB        string `yaml:"meta_db" doc:"DSN of the metadata database used in fleet mode to persist cross-target state. Blank in standalone mode."`
	EncryptionKey string `yaml:"encryption_key" doc:"Passphrase that encrypts stored secrets: per-database passwords, channel secrets and API-set config secrets. Also SAGE_ENCRYPTION_KEY(_FILE)." secret:"true"`
	// EncryptionKeyPrevious opens config secrets sealed under the key
	// before a rotation; startup re-seals them under encryption_key.
	EncryptionKeyPrevious string `yaml:"encryption_key_previous" doc:"Previous encryption_key during a rotation: config secrets sealed under it are re-encrypted at startup. Remove once rotated." secret:"true"`
	// DataDir is a persistent directory pg_sage owns. Without an
	// encryption_key, startup generates <data_dir>/encryption.key there.
	DataDir string `yaml:"data_dir" doc:"Persistent directory owned by pg_sage. Without encryption_key, pg_sage generates data_dir/encryption.key (mode 600) and seals stored secrets with it; back the file up. Also SAGE_DATA_DIR."`

	// Legacy env-var fields
	APIKey  string `yaml:"-"`
	TLSCert string `yaml:"-"`
	TLSKey  string `yaml:"-"`

	// Runtime (not from config file)
	ConfigPath         string `yaml:"-"`
	PGVersionNum       int    `yaml:"-"` // e.g. 160000 for PG16
	HasWALColumns      bool   `yaml:"-"`
	HasPlanTimeColumns bool   `yaml:"-"`
	StatementsSchema   string `yaml:"-"` // pg_stat_statements' schema; empty: not found
	CloudEnvironment   string `yaml:"-"` // e.g. "rds", "cloud-sql", "self-managed"

	// The file layer's presence set and the overrides masking it (config-03,
	// explicit_paths.go). Both are immutable once set, so Clone shares them.
	explicitPaths map[string]struct{}
	overrideMask  *overrideMask
}

type PostgresConfig struct {
	Host           string `yaml:"host" doc:"Hostname or IP of the target PostgreSQL instance. Ignored when database_url is set."`
	Port           int    `yaml:"port" doc:"TCP port of the target PostgreSQL instance. Ignored when database_url is set."`
	User           string `yaml:"user" doc:"PostgreSQL login role used by the sidecar. Requires pg_read_all_stats plus write access to the sage schema."`
	Password       string `yaml:"password" doc:"Password for the sidecar login role. Prefer environment-variable substitution or a secrets manager over committing literal values." secret:"true"`
	Database       string `yaml:"database" doc:"Target database name the sidecar connects to. Ignored when database_url is set."`
	SSLMode        string `yaml:"sslmode" doc:"libpq sslmode string (disable, allow, prefer, require, verify-ca, verify-full). Use verify-full in production."`
	MaxConnections int    `yaml:"max_connections" doc:"Maximum connections the sidecar pgx pool will open to this target. Keep well below max_connections on the server."`
	DatabaseURL    string `yaml:"database_url" doc:"Full libpq connection URL. When set, overrides host/port/user/password/database/sslmode."`
	ActionDSN      string `yaml:"action_dsn" doc:"Optional DSN of a second role on the same database. Only executor actions and backend cancel/terminate use it (a lazy pool of at most 7); the main role only observes. Empty: one role." secret:"true"`
	Provider       string `yaml:"provider" doc:"Optional provider override, e.g. rds, cloud-sql, neon or self-managed. Used only while detection cannot tell; a disagreement with detection keeps the detected provider and warns."`
	// CloudResource names the provider resource when the host does not.
	CloudResource CloudResourceConfig `yaml:"cloud_resource"`
	// CNPG names the CloudNativePG Cluster behind the database (market-12).
	CNPG CNPGConfig `yaml:"cnpg"`
}

type CollectorConfig struct {
	IntervalSeconds int `yaml:"interval_seconds"`
	BatchSize       int `yaml:"batch_size"`
	MaxQueries      int `yaml:"max_queries"`
}

type AnalyzerConfig struct {
	IntervalSeconds              int     `yaml:"interval_seconds" doc:"How often the analyzer runs its rule checks, in seconds. Lower values detect issues faster but add load."`
	SlowQueryThresholdMs         int     `yaml:"slow_query_threshold_ms" doc:"Queries whose mean execution time over the last collector interval is above this are considered slow and become candidates for optimization findings."`
	SlowQueryMinCalls            int     `yaml:"slow_query_min_calls" doc:"Fewest calls a query must make in one collector interval before its interval mean is judged by slow_query and query_regression. 0 uses the default. Default: 5."`
	SeqScanMinRows               int     `yaml:"seq_scan_min_rows" doc:"Minimum table row count before a sequential scan is reported as a finding. Below this, a seq scan is usually cheaper than an index lookup."`
	UnusedIndexWindowDays        int     `yaml:"unused_index_window_days" doc:"An index must be unused (idx_scan=0) for at least this many days before the analyzer suggests dropping it. Default 7 days." warning:"Setting this too low can recommend dropping indexes that support infrequent but critical queries (e.g. monthly reports)."`
	IndexBloatThresholdPct       int     `yaml:"index_bloat_threshold_pct" doc:"Estimated btree bloat percentage (0-100) for an index_bloat finding recommending REINDEX INDEX CONCURRENTLY: info from it, warning from twice it. 0 uses the default. Default: 30."`
	IndexBloatMinBytes           int64   `yaml:"index_bloat_min_bytes" doc:"Smallest btree index (bytes, from pg_class.relpages) whose bloat the collector estimates from planner statistics each cycle, at most the 200 largest. 0 uses the default. Default: 104857600 (100 MiB)."`
	TableBloatDeadTuplePct       int     `yaml:"table_bloat_dead_tuple_pct" doc:"Dead-tuple percentage threshold for reporting table bloat. Above this the analyzer emits a vacuum or repack finding."`
	TableBloatMinRows            int     `yaml:"table_bloat_min_rows" doc:"Minimum row count before a table is considered for bloat analysis. Tiny tables are noisy and not worth reporting."`
	TableBloatMinBytes           int64   `yaml:"table_bloat_min_bytes" doc:"Minimum heap size in bytes before the freeze custodian plans an online bloat rebuild from the catalog bloat estimate. 0 uses the default. Default: 1073741824 (1 GiB)."`
	AutovacuumTuneMinRows        int     `yaml:"autovacuum_tune_min_rows" doc:"Minimum live-row count before the per-table autovacuum tuner recommends a lower scale factor. Tuning small tables is pointless. Default: 1000000."`
	AnalyzeStaleMinRows          int     `yaml:"analyze_stale_min_rows" doc:"Minimum live rows before recommending ANALYZE on a stale/never-analyzed table. Default: 10000."`
	AnalyzeStaleDays             int     `yaml:"analyze_stale_days" doc:"A table whose last (auto)analyze is older than this many days, with write activity, is flagged for ANALYZE. Default: 7."`
	WraparoundFreezeShare        float64 `yaml:"wraparound_freeze_share" doc:"Share (0-1] of a table's effective freeze maximum (setting or lower reloption) at which the analyzer reports wraparound risk; the freeze custodian decides the VACUUM. Default: 0.75."`
	IdleInTxTimeoutMinutes       int     `yaml:"idle_in_transaction_timeout_minutes" doc:"Report sessions sitting in 'idle in transaction' longer than this. Long idle-in-tx sessions block vacuum and hold locks."`
	CacheHitRatioWarning         float64 `yaml:"cache_hit_ratio_warning" doc:"Warn when the shared buffers cache hit ratio falls below this fraction (0.0-1.0). Low ratios suggest shared_buffers is under-sized or working set is too large."`
	XIDWraparoundWarning         int64   `yaml:"xid_wraparound_warning" doc:"Transaction age at which the analyzer emits a wraparound warning. Normally ~200 million."`
	XIDWraparoundCritical        int64   `yaml:"xid_wraparound_critical" doc:"Transaction age at which the analyzer escalates to critical severity. Normally ~1.5 billion — past this autovacuum will force emergency wraparound runs." warning:"If this is hit, autovacuum enters aggressive mode and the database may eventually refuse writes."`
	RegressionThresholdPct       int     `yaml:"regression_threshold_pct" doc:"Percent latency regression required before a query is flagged as a regressor. Set low for sensitive detection, higher to reduce noise."`
	RegressionLookbackDays       int     `yaml:"regression_lookback_days" doc:"How many days of pg_stat_statements history the regression detector compares against."`
	CheckpointFreqWarningPerHour int     `yaml:"checkpoint_frequency_warning_per_hour" doc:"Warn when checkpoints occur more than this many times per hour. High frequency indicates max_wal_size is too small."`

	// v0.8.5 Feature 3 — work_mem role-promotion advisor.
	// Threshold: how many hint applications in a 24h window on a single
	// role/query_hash before promoting to a role-level work_mem finding.
	WorkMemPromotionThreshold int `yaml:"work_mem_promotion_threshold" doc:"How many active work_mem hints a single role must accumulate before the analyzer recommends promoting work_mem at the role level via ALTER ROLE. Set 0 to disable the advisor."`

	// v0.9.2 — Slow active replication slot threshold (bytes).
	SlowSlotRetainedBytes int64 `yaml:"slow_slot_retained_bytes" doc:"Active slots retaining more WAL than this (bytes) are flagged as slow consumers. Default 1 GB. Slow consumers hold xmin, blocking vacuum."`

	// InactiveSlotMinMinutes: see inactive_slot.go.
	InactiveSlotMinMinutes int `yaml:"inactive_slot_min_minutes" doc:"Report a replication slot once it has been inactive this many minutes (wal_status extended, unreserved or lost reports it at once). 0 uses the default. Range 0-10080. Default: 5."`

	// Subscriber and CDC health thresholds: see replication.go (market-13).
	SubscriptionLagMinutes int `yaml:"subscription_lag_minutes" doc:"Raise subscription_lag when a subscription's last report to its publisher (pg_stat_subscription.latest_end_time) is this many minutes old. 0 uses the default. Range 0-10080. Default: 5."`
	CDCStallMinutes        int `yaml:"cdc_stall_minutes" doc:"Raise cdc_consumer_stalled when WAL advanced but an active logical slot's confirmed_flush_lsn did not move for this many minutes. 0 uses the default. Range 0-10080. Default: 10."`

	// v0.9 — Lock chain detection.
	LockChain LockChainConfig `yaml:"lock_chain"`

	// Schema guard post-DDL debounce: see schema_guard.go.
	SchemaGuardDDLDebounceSeconds int `yaml:"schema_guard_ddl_debounce_seconds" doc:"After pg_sage's own DDL, the schema guard re-scans the schema at most once per this many seconds (DDL bursts are coalesced into one extra scan). 0 uses the default. Range 0-3600. Default: 60."`

	// SelfCostBudgetMs: see self_cost.go.
	SelfCostBudgetMs int `yaml:"self_cost_budget_ms" doc:"Raise a sage_self_cost finding when pg_sage's own statements use more than this many ms of database time per collector cycle. 0 disables the finding. Range 0-3600000. Default: 3000."`

	// DisabledRules: see disabled_rules.go (market-11).
	DisabledRules []string `yaml:"disabled_rules" doc:"Analyzer rules or categories to skip; their open findings resolve as rule_disabled. The wraparound, sequence and sage_* rules cannot be disabled. Default: empty."`
}

type SafetyConfig struct {
	CPUCeilingPct            int `yaml:"cpu_ceiling_pct" doc:"Skip collector cycles when active client sessions / max_connections exceeds this percent (1-100), so pg_sage adds no load during incidents. Verify inherits it while host_cpu_ceiling_pct is unset."`
	HostCPUCeilingPct        int `yaml:"host_cpu_ceiling_pct" doc:"Index verification applies a change only while database-host CPU is at or below this percent (1-100). Unset (0) inherits cpu_ceiling_pct, default 90."`
	QueryTimeoutMs           int `yaml:"query_timeout_ms" doc:"statement_timeout applied to every collector and analyzer query issued by the sidecar. Protects the target from runaway introspection."`
	DDLTimeoutSeconds        int `yaml:"ddl_timeout_seconds" doc:"Timeout applied to DDL actions (CREATE INDEX, ALTER TABLE) executed by the executor. Per-statement ceiling, not per-cycle."`
	DiskPressureThresholdPct int `yaml:"disk_pressure_threshold_pct" doc:"When database data volume usage exceeds this percent, skip write-producing actions. Prevents adding indexes to a near-full volume."`
	BackoffConsecutiveSkips  int `yaml:"backoff_consecutive_skips" doc:"After this many consecutive skipped cycles (e.g. CPU ceiling hit repeatedly), enter dormant mode and slow the cadence until load subsides."`
	DormantIntervalSeconds   int `yaml:"dormant_interval_seconds" doc:"Cycle interval (seconds) used while in dormant mode — typically much larger than the normal interval so the sidecar wakes rarely while the target is stressed."`
	LockTimeoutMs            int `yaml:"lock_timeout_ms" doc:"lock_timeout applied to connections running DDL or ANALYZE. Must be > 0 for ANALYZE actions in v0.8.5 — autovacuum can hold a ShareUpdateExclusiveLock indefinitely." warning:"Zero disables the timeout entirely and is refused by the executor for ANALYZE actions."`

	// IO ceilings for declared-capacity load admission (D6), independent
	// of cpu_ceiling_pct.
	DataIOCeilingPct int `yaml:"data_io_ceiling_pct" doc:"Data IO ceiling, % of capacity."`
	WALIOCeilingPct  int `yaml:"wal_io_ceiling_pct" doc:"WAL IO ceiling, % of capacity."`

	// Per-class statement timeouts (executor-05): see ddl_class_timeouts.go.
	IndexBuildTimeoutSeconds int `yaml:"index_build_timeout_seconds" doc:"statement_timeout of a CREATE INDEX CONCURRENTLY run by the executor (seconds). Other DDL keeps ddl_timeout_seconds. 0 uses the default. Range 60-86400. Default: 14400."`
	ReindexTimeoutSeconds    int `yaml:"reindex_timeout_seconds" doc:"statement_timeout of a REINDEX CONCURRENTLY run by the executor (seconds). 0 uses the default. Range 60-86400. Default: 14400."`
	VacuumTimeoutSeconds     int `yaml:"vacuum_timeout_seconds" doc:"Cap on a VACUUM (not VACUUM FULL) run by the executor (seconds), enforced by its watchdog; the VACUUM runs with statement_timeout 0. 0 uses the default. Range 60-86400. Default: 14400."`
	// VACUUM watchdog (vacuum-05): see vacuum_stall.go.
	VacuumStallMinutes int `yaml:"vacuum_stall_minutes" doc:"Minutes an executor VACUUM may show no heap-phase progress before its watchdog cancels it (vacuum_stalled). 0 uses the default. Range 5-240. Default: 15."`
}

type TrustConfig struct {
	Level                 string `yaml:"level" doc:"Operator ceiling and kill switch, never a grant: the ledger grants, the operator caps. observation: cases only; advisory: earned safe actions, moderate to approval; autonomous: both." warning:"Changing trust can expand what execution_mode=auto may execute without human review."`
	RampStart             string `yaml:"ramp_start" doc:"RFC3339 time pg_sage began observing the database; auto-persisted on first startup if empty. Observation time is the floor before a trust promotion can be proposed."`
	RampSafeHours         int    `yaml:"ramp_safe_hours" doc:"Minimum hours observed before pg_sage may propose a promotion to L2 (or L3 for SAFE classes): a floor, never a grant. 1-8760. Default: 192. Irreversible actions: at least 192." warning:"Lower values let pg_sage propose promotions sooner after first start; the sidecar warns at startup."`
	RampModerateHours     int    `yaml:"ramp_moderate_hours" doc:"Minimum hours observed before pg_sage may propose a MODERATE class's promotion to L3: a floor, never a grant. 1-8760, >= ramp_safe_hours. Default: 744. Irreversible: at least 744." warning:"Lower values let pg_sage propose promotions sooner after first start; the sidecar warns at startup."`
	MaintenanceWindow     string `yaml:"maintenance_window" doc:"Window for MODERATE auto-actions: always, never, presets (nights, weeknights, weekends), Mon-Fri 01:00-05:00, or cron (1h wide, or @30m). Optional IANA zone: weeknights America/Chicago." example:"weeknights"`
	Tier3Safe             bool   `yaml:"tier3_safe" doc:"Operator switch for unattended SAFE actions (ANALYZE, non-FULL VACUUM) the trust ledger has earned; off caps them at approval. Needs execution_mode=auto and trust.level advisory+."`
	Tier3Moderate         bool   `yaml:"tier3_moderate" doc:"Operator switch for unattended MODERATE actions (CREATE INDEX CONCURRENTLY) the trust ledger has earned; off caps them at approval. Needs execution_mode=auto, trust.level=autonomous." warning:"Moderate actions can consume IO or briefly contend for locks."`
	Tier3HighRisk         bool   `yaml:"tier3_high_risk" doc:"Enable tier-3 actions classified as high risk. Ignored outside fleet mode — standalone always forces this to false." mode:"fleet-only" warning:"High-risk actions can destabilize production — enable only with a reviewed rollback plan."`
	RollbackThresholdPct  int    `yaml:"rollback_threshold_pct" doc:"Latency regression percentage (0-100) that triggers automatic rollback of a recently applied action. Lower = more sensitive."`
	RollbackWindowMinutes int    `yaml:"rollback_window_minutes" doc:"How many minutes after applying an action pg_sage watches for regressions before considering it stable."`
	RollbackCooldownDays  int    `yaml:"rollback_cooldown_days" doc:"After a rollback, the same action family is blocked for this many days to prevent flap. Default 3 days."`
	CascadeCooldownCycles int    `yaml:"cascade_cooldown_cycles" doc:"If N consecutive cycles each propose new actions, throttle to observe impact. Prevents storm-like cascades of small changes."`
}

type LLMConfig struct {
	Auth                   string               `yaml:"auth" doc:"LLM authentication: api_key (default) or databricks (SDK unified authentication with automatic token refresh). Databricks auth requires the configured workspace HTTPS origin."`
	Enabled                bool                 `yaml:"enabled" doc:"Global enable for LLM-assisted analysis. On by default; calls a provider only once llm.endpoint and llm.api_key are set. When off, pg_sage uses deterministic heuristics only."`
	Endpoint               string               `yaml:"endpoint" doc:"LLM provider base URL. Supports OpenAI-compatible chat-completions endpoints. Can be left blank to use the vendor default."`
	APIKey                 string               `yaml:"api_key" doc:"Authentication token for the LLM provider. Prefer sourcing from environment via ${LLM_API_KEY} rather than committing literal values." secret:"true"`
	Model                  string               `yaml:"model" doc:"Model identifier sent to the provider (e.g. gpt-4o-mini). Must support JSON-mode responses for structured outputs."`
	TimeoutSeconds         int                  `yaml:"timeout_seconds" doc:"Timeout of each LLM request attempt; 429 and 5xx replies are retried, up to 4 attempts. Reasoning models get at least 120 s per attempt. Low values protect the analyzer cycle."`
	TokenBudgetDaily       int                  `yaml:"token_budget_daily" doc:"Daily token cap (input + output) per LLM client: the shared client and each database's general and optimizer client get their own copy. llm.global_token_budget_daily caps the sum."`
	FleetTokenBudgetDaily  int                  `yaml:"fleet_token_budget_daily" doc:"Fleet-wide daily token cap split per database so one noisy database can't drain the whole budget. 0 disables per-database budgeting (fleet mode only)."`
	GlobalTokenBudgetDaily int                  `yaml:"global_token_budget_daily" doc:"Process-wide daily token cap across all LLM clients. 0 derives today's total: the clients' token_budget_daily summed, an active fleet budget standing in for the per-database ones."`
	ContextBudgetTokens    int                  `yaml:"context_budget_tokens" doc:"Trim target in tokens (~4 bytes each) for prompt context: lowering it shrinks advisor, tuning and explain prompts, each keeping its own cap. 0 turns off this extra trim. Never refuses or caps answers."`
	CooldownSeconds        int                  `yaml:"cooldown_seconds" doc:"Dedup window and breaker cooldown: an identical request is not re-sent within this many seconds; after 3 failures in a row (or one 401/403) calls pause this long, then one probe decides."`
	JSONMode               bool                 `yaml:"json_mode" doc:"When true, requests structured JSON via response_format: json_object. Supported by OpenAI, Gemini (OpenAI-compat), Groq, Ollama. Off for providers that reject unknown fields."`
	TokenParameter         string               `yaml:"token_parameter" doc:"Completion cap parameter: auto (max_tokens, switching per model to max_completion_tokens when the provider asks), max_tokens or max_completion_tokens. Default: auto."`
	ToolReasoningEffort    string               `yaml:"tool_reasoning_effort" doc:"reasoning_effort sent with tool calls: auto (unset; none once a model rejects tools without it), none, low, medium, high, or omit (never sent). Default: auto."`
	IndexOptimizer         IndexOptimizerConfig `yaml:"index_optimizer"` // Deprecated: use Optimizer.
	Optimizer              OptimizerConfig      `yaml:"optimizer"`
	OptimizerLLM           OptimizerLLMConfig   `yaml:"optimizer_llm"`
	RedactNumericLiterals  bool                 `yaml:"redact_numeric_literals" doc:"Replace compared numbers of 6+ digits (=, <>, <, >, IN, ANY, BETWEEN, Key (c)=(...)) with ? before SQL, plan or log text reaches a model. Short numbers are kept. Default: true."`
	Pricing                map[string]LLMPrice  `yaml:"pricing" doc:"Optional USD prices per million tokens by model id: {model: {input_per_m, output_per_m, reasoning_per_m}}. Empty by default; an unpriced model gets a NULL cost, never a guess."`
}

type IndexOptimizerConfig struct {
	Enabled            bool `yaml:"enabled"`
	MinQueryCalls      int  `yaml:"min_query_calls"`
	MaxIndexesPerTable int  `yaml:"max_indexes_per_table"`
	MaxIncludeColumns  int  `yaml:"max_include_columns"`
	WriteHeavyRatio    int  `yaml:"write_heavy_ratio_pct"`
}

// OptimizerConfig controls the index optimizer v2 behavior.
type OptimizerConfig struct {
	Enabled              bool    `yaml:"enabled" doc:"Allow index creates and drops: the tuning agent (needs an LLM) and, without one, the deterministic index floor. Changes execute only via the policy gate and trust level. Default: true."`
	MinQueryCalls        int     `yaml:"min_query_calls"`
	MaxIndexesPerTable   int     `yaml:"max_indexes_per_table"`
	MaxNewPerTable       int     `yaml:"max_new_per_table"`
	MaxIncludeColumns    int     `yaml:"max_include_columns" doc:"Most INCLUDE columns a proposed index may carry; a wider proposal is refused with a reason naming the cap. 0 or less means 3. Default: 3."`
	WriteHeavyRatioPct   int     `yaml:"write_heavy_ratio_pct"`
	MinSnapshots         int     `yaml:"min_snapshots"`
	HypoPGMinImprovePct  float64 `yaml:"hypopg_min_improvement_pct"`
	ConfidenceThreshold  float64 `yaml:"confidence_threshold"`
	WriteImpactThreshPct float64 `yaml:"write_impact_threshold_pct"`
	// RejectionMemory: optimizer_rejection_memory.go.
	RejectionMemory OptimizerRejectionMemoryConfig `yaml:"rejection_memory"`
}

// OptimizerLLMConfig configures the dedicated optimizer LLM (reasoning-tier).
type OptimizerLLMConfig struct {
	Enabled           bool   `yaml:"enabled" doc:"Enable the dedicated reasoning-tier LLM used by the index optimizer. When false the optimizer falls back to the general llm.* client. Default: false."`
	Endpoint          string `yaml:"endpoint" doc:"Base URL for the optimizer LLM provider. Can differ from the general llm.endpoint when the reasoning tier lives on another provider."`
	APIKey            string `yaml:"api_key" doc:"API key for the optimizer LLM. Prefer env-var substitution over literal values." secret:"true"`
	Model             string `yaml:"model" doc:"Reasoning-tier model identifier (e.g. o1-mini). Must return JSON when requested."`
	TimeoutSeconds    int    `yaml:"timeout_seconds" doc:"Per-request timeout for optimizer LLM calls. Reasoning models typically need longer timeouts than chat models."`
	TokenBudgetDaily  int    `yaml:"token_budget_daily" doc:"Daily token cap for the optimizer LLM — independent of the general llm.token_budget_daily so reasoning spend can be tracked separately."`
	CooldownSeconds   int    `yaml:"cooldown_seconds" doc:"As llm.cooldown_seconds, for the optimizer client: an identical request is not re-sent within this many seconds, and the breaker pauses calls this long after 3 failures. 0 uses llm.cooldown_seconds."`
	MaxOutputTokens   int    `yaml:"max_output_tokens" doc:"Upper bound on output tokens accepted from the optimizer LLM. Truncates runaway responses."`
	FallbackToGeneral bool   `yaml:"fallback_to_general" doc:"If true, fall back to the general llm.* client when the optimizer LLM is unavailable or over budget."`
}

type AdvisorConfig struct {
	Enabled           bool `yaml:"enabled" doc:"Run the LLM configuration advisor. On by default; needs a usable LLM at startup. Its changes execute only through the policy gate and trust level. Default: true."`
	IntervalSeconds   int  `yaml:"interval_seconds"`
	VacuumEnabled     bool `yaml:"vacuum_enabled" doc:"Let the tuning agent propose table storage parameters (autovacuum, fillfactor) for its write-amplification cases. Needs advisor.enabled. Default: true."`
	WALEnabled        bool `yaml:"wal_enabled"`
	ConnectionEnabled bool `yaml:"connection_enabled"`
	MemoryEnabled     bool `yaml:"memory_enabled" doc:"Let the tuning agent propose server settings (work_mem and other memory, planner and autovacuum settings) for its cases. Needs advisor.enabled. Default: true."`
	RewriteEnabled    bool `yaml:"rewrite_enabled"`
	BloatEnabled      bool `yaml:"bloat_enabled"`
	BudgetProposals   bool `yaml:"budget_proposals" doc:"Propose a workload budget statement_timeout per role from its statement time (3x the 14-day p99, rounded up, never below the observed maximum) as info findings. Default: true."`
}

type BriefingConfig struct {
	Schedule        string   `yaml:"schedule"`
	Channels        []string `yaml:"channels"`
	SlackWebhookURL string   `yaml:"slack_webhook_url"`
	FleetMode       string   `yaml:"fleet_mode" doc:"Fleet and meta-db modes: rollup (default) posts one briefing message covering every database; per_database posts one per database. Each database still stores its own briefing. Restart to change."`
}

type AlertingConfig struct {
	Enabled              bool            `yaml:"enabled"`
	CheckIntervalSeconds int             `yaml:"check_interval_seconds"`
	CooldownMinutes      int             `yaml:"cooldown_minutes"`
	RepeatMinutes        map[string]int  `yaml:"repeat_minutes" doc:"Minutes between reminders of a finding that stays open, per severity. A missing key takes its default; 0 never reminds. Default: critical 240, warning 1440, info 0."`
	QuietHoursStart      string          `yaml:"quiet_hours_start"`
	QuietHoursEnd        string          `yaml:"quiet_hours_end"`
	Timezone             string          `yaml:"timezone"`
	SlackWebhookURL      string          `yaml:"slack_webhook_url"`
	PagerDutyRoutingKey  string          `yaml:"pagerduty_routing_key"`
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
	Enabled                    bool    `yaml:"enabled" doc:"Enable the root cause analysis engine. Correlates multiple signals into unified incident narratives."`
	LLMCorrelationThreshold    int     `yaml:"llm_correlation_threshold" doc:"Minimum concurrent signals to trigger LLM-based correlation (Tier 2). Default: 3."`
	DedupWindowMinutes         int     `yaml:"dedup_window_minutes" doc:"Time window for deduplicating incidents with the same signals and root object. Default: 30."`
	EscalationCycles           int     `yaml:"escalation_cycles" doc:"Consecutive analyzer cycles an incident stays open before escalating warning to critical. Default: 5."`
	ResolutionCycles           int     `yaml:"resolution_cycles" doc:"Consecutive clear cycles before an incident auto-resolves. Default: 2."`
	StaleAfterHours            int     `yaml:"stale_after_hours" doc:"Hours an open incident may go unseen before pg_sage resolves it as stale (no notification when it was last seen that long ago). 1-8760, at least the dedup window. Default: 24."`
	VacuumMinDeadTuples        int     `yaml:"vacuum_min_dead_tuples" doc:"Fewest dead tuples a table needs before its dead-tuple ratio counts toward the vacuum_blocked (autovacuum falling behind) incident. 1-1000000000. Default: 1000."`
	VacuumMinTableMB           int     `yaml:"vacuum_min_table_mb" doc:"Smallest heap, in MB, a table needs before its dead-tuple ratio counts toward the vacuum_blocked incident. 1-1048576. Default: 8."`
	XminHolderMinMinutes       int     `yaml:"xmin_holder_min_minutes" doc:"Minutes a session's transaction must be open before the vacuum_blocked incident names it as the holder of the oldest xmin. 1-1440. Default: 5."`
	PreparedXactMinAgeMinutes  int     `yaml:"prepared_xact_min_age_minutes" doc:"Minutes a prepared transaction of this database must be old before it opens orphaned_prepared_tx or is named the vacuum_blocked holder. 1-10080. Default: 30."`
	ConnectionSaturationPct    int     `yaml:"connection_saturation_pct" doc:"Percentage of max_connections that triggers the connections_high signal. Default: 80."`
	ReplicationLagThresholdS   int     `yaml:"replication_lag_threshold_seconds" doc:"Seconds of replay lag before the replication_lag_increasing signal fires. Default: 30."`
	WALSpikeMultiplier         float64 `yaml:"wal_spike_multiplier" doc:"wal_growth_spike fires when the WAL rate (insert position advance per minute) of a collector interval is this many times the previous interval's and at least 1 MiB/min. Default: 2.0."`
	LockChainIntervalSeconds   int     `yaml:"lock_chain_interval_seconds" doc:"Seconds between lock-chain fast-path checks, which open or update the lock_contention incident between analyzer cycles. 0 disables the fast path; otherwise 10-3600. Default: 60."`
	NarrationEnabled           bool    `yaml:"narration_enabled" doc:"Add an LLM narrative, citing the incident's evidence and catalog probe results, to incident notifications. Off, no LLM, or any LLM failure uses the deterministic summary. Default: true."`
	LogFastPathIntervalSeconds int     `yaml:"log_fast_path_interval_seconds" doc:"Seconds between log fast-path passes, which open incidents for critical log lines (PANIC, disk full, OOM, wraparound) between analyzer cycles. 0 disables; otherwise 1-3600. Default: 5."`
	LogQuietMinutes            int     `yaml:"log_quiet_minutes" doc:"Minutes without a new log line before a log incident auto-resolves (not clear cycles). 0 = rca.dedup_window_minutes; else 1-1440, within stale_after_hours. Default: 0."`
	RecurrenceRenotifyHours    int     `yaml:"recurrence_renotify_hours" doc:"Hours after a log incident resolves in which a recurrence at no higher severity is linked but not notified again. 1-168. Default: 6."`
}

// LockChainConfig controls lock chain detection (v0.9).
type LockChainConfig struct {
	Enabled                  bool     `yaml:"enabled" doc:"Enable lock chain detection using pg_blocking_pids(). Detects cascading lock waits."`
	MinBlockedThreshold      int      `yaml:"min_blocked_threshold" doc:"Minimum blocked sessions before a lock chain generates a finding. Default: 3."`
	CriticalBlockedThreshold int      `yaml:"critical_blocked_threshold" doc:"Blocked session count that escalates finding to critical severity. Default: 10."`
	IdleInTxTerminateMinutes int      `yaml:"idle_in_tx_terminate_minutes" doc:"Minutes an idle-in-transaction root blocker must wait before pg_terminate_backend is recommended. Default: 5."`
	ActiveQueryCancelMinutes int      `yaml:"active_query_cancel_minutes" doc:"Minutes an active query root blocker must run before pg_cancel_backend is recommended. Default: 15."`
	SafePatterns             []string `yaml:"safe_patterns" doc:"Application name substrings that protect a process from automated termination/cancellation."`
}

// LogWatchConfig controls PostgreSQL log file monitoring (v0.9.1).
type LogWatchConfig struct {
	Enabled             bool     `yaml:"enabled" doc:"Enable RCA logs from a configured provider API or local PostgreSQL jsonlog/csvlog files. Requires rca.enabled=true. Default: false."`
	LogDirectory        string   `yaml:"log_directory" doc:"Path to PG log directory. Auto-detected from pg_settings if blank."`
	Format              string   `yaml:"format" doc:"Log format: jsonlog (PG15+) or csvlog. Auto-detected if blank."`
	PollIntervalMs      int      `yaml:"poll_interval_ms" doc:"Log file poll interval (the tailer only polls). Default: 1000."`
	DedupWindowS        int      `yaml:"dedup_window_seconds" doc:"Suppress duplicate log signals within this window. Default: 60."`
	MaxLineLenBytes     int      `yaml:"max_line_len_bytes" doc:"Drop log records longer than this (an auto_explain plan record may reach 1 MiB); drops are counted as oversized_record. Default: 65536."`
	TempFileMinBytes    int      `yaml:"temp_file_min_bytes" doc:"Minimum temp file size to trigger log_temp_file_created. Default: 10485760."`
	MaxLinesPerCycle    int      `yaml:"max_lines_per_cycle" doc:"Stop parsing after this many lines per drain cycle. Default: 10000."`
	ExcludeApplications []string `yaml:"exclude_applications" doc:"Application names excluded from timeout/deadlock signals. Default: pg_sage."`
	SlowQueryEnabled    bool     `yaml:"slow_query_enabled" doc:"Opt-in: parse log_min_duration_statement output as log_slow_query signals. Default: false."`
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
	Enabled      bool            `yaml:"enabled" doc:"Enable policy-based runaway query detection and escalation (warn -> cancel -> terminate)."`
	Policies     []RunawayPolicy `yaml:"policies" doc:"List of runaway query policies. A query matching any policy enters the escalation state machine."`
	SafePatterns []string        `yaml:"safe_patterns" doc:"Application name substrings protected from runaway termination. Merged with lock_chain.safe_patterns at startup."`
}

// ExplainConfig controls the natural language EXPLAIN feature (v0.9).
type ExplainConfig struct {
	Enabled         bool `yaml:"enabled" doc:"Enable the LLM-powered natural language EXPLAIN API endpoint."`
	TimeoutMs       int  `yaml:"timeout_ms" doc:"statement_timeout for EXPLAIN ANALYZE execution. Default: 10000."`
	CacheTTLMinutes int  `yaml:"cache_ttl_minutes" doc:"TTL for cached explain results. Default: 60."`
	MaxTokens       int  `yaml:"max_tokens" doc:"Maximum LLM output tokens for explain responses. Default: 4096."`
}

// SchemaLintConfig controls schema anti-pattern detection (v0.10).
type SchemaLintConfig struct {
	Enabled             bool     `yaml:"enabled" doc:"Enable periodic schema anti-pattern scanning."`
	ScanIntervalMinutes int      `yaml:"scan_interval_minutes" doc:"How often to run the full schema lint scan. Default: 60."`
	IncludeSchemas      []string `yaml:"include_schemas" doc:"Schemas to scan. Empty means all non-system schemas."`
	ExcludeSchemas      []string `yaml:"exclude_schemas" doc:"Schemas to exclude. Default: pg_catalog, information_schema."`
	DisabledRules       []string `yaml:"disabled_rules" doc:"Rule IDs to skip (e.g. lint_serial_usage)."`
	MinTableRows        int      `yaml:"min_table_rows" doc:"Ignore findings on tables below this row count. Default: 1000."`
}

// MigrationConfig controls the DDL safety advisor (v0.10).
type MigrationConfig struct {
	Enabled               bool   `yaml:"enabled" doc:"Enable DDL safety advisory."`
	Mode                  string `yaml:"mode" doc:"Operating mode: advisory (default). Active mode deferred to future release."`
	LogDetection          bool   `yaml:"log_detection" doc:"Parse DDL from PG logs (requires log_statement=ddl)."`
	ActivityPolling       bool   `yaml:"activity_polling" doc:"Scan pg_stat_activity for active DDL."`
	PollIntervalSeconds   int    `yaml:"poll_interval_seconds" doc:"How often to poll during active DDL. Default: 5."`
	DDLRowThreshold       int    `yaml:"ddl_row_threshold" doc:"Row count below which DDL is considered low-risk. Default: 10000."`
	QueueWaitersThreshold int    `yaml:"queue_waiters_threshold" doc:"Sessions queued directly behind an in-flight DDL waiting for its lock before the finding turns critical with approval-only cancel guidance. Needs the RCA fast path. Default: 3."`
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
	SnapshotsDays int `yaml:"snapshots_days"`
	FindingsDays  int `yaml:"findings_days"`
	ActionsDays   int `yaml:"actions_days"`
	ExplainsDays  int `yaml:"explains_days"`
	DecisionsDays int `yaml:"decisions_days" doc:"Days to keep parked, queued, blocked and observe-only decisions after they were last seen; ones behind an action or verification are kept. 0 disables. Range 0-3650. Default 30."`
	AuthAuditDays int `yaml:"auth_audit_days" doc:"Days to keep the sign-in audit trail (sage.auth_audit: SSO logins, links, grants and break-glass use). 0 keeps it forever. Range 0-3650. Default 365."`
	// SageSizeWarningPct: see sage_footprint.go.
	SageSizeWarningPct int `yaml:"sage_size_warning_pct" doc:"Raise a sage_footprint finding when pg_sage's own tables (the sage schema) exceed this percent of the database size; below 256 MB it is never a finding. 0 disables the check. Default 10."`
	// QueryStoreDays and SnapshotsMaxPct: see storage_retention.go.
	QueryStoreDays  int `yaml:"query_store_days" doc:"Days to keep per-query samples in sage.query_store (its readers look back at most 7 days). 0 disables. Range 0-3650. Default 14."`
	SnapshotsMaxPct int `yaml:"snapshots_max_pct" doc:"Cap sage.snapshots at this percent of the database size (never below 256 MB): the oldest days are removed first. 0 disables the cap. Range 0-100. Default 5."`
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

// Interval helpers.
func (c *CollectorConfig) Interval() time.Duration {
	return time.Duration(c.IntervalSeconds) * time.Second
}

func (c *AnalyzerConfig) Interval() time.Duration {
	return time.Duration(c.IntervalSeconds) * time.Second
}

func (c *AdvisorConfig) Interval() time.Duration {
	return time.Duration(c.IntervalSeconds) * time.Second
}

func (c *SafetyConfig) DormantInterval() time.Duration {
	return time.Duration(c.DormantIntervalSeconds) * time.Second
}

func (c *AlertingConfig) CheckInterval() time.Duration {
	return time.Duration(c.CheckIntervalSeconds) * time.Second
}

func (c *AutoExplainConfig) CollectInterval() time.Duration {
	return time.Duration(c.CollectIntervalSeconds) * time.Second
}

func (c *SafetyConfig) QueryTimeout() time.Duration {
	return time.Duration(c.QueryTimeoutMs) * time.Millisecond
}

func (c *SafetyConfig) DDLTimeout() time.Duration {
	return time.Duration(c.DDLTimeoutSeconds) * time.Second
}

func (c *SafetyConfig) LockTimeout() int {
	if c.LockTimeoutMs <= 0 {
		return DefaultLockTimeoutMs
	}
	return c.LockTimeoutMs
}

// DSN builds a libpq connection string from individual fields.
func (p *PostgresConfig) DSN() string {
	if p.DatabaseURL != "" {
		return p.DatabaseURL
	}
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		conninfoValue(p.Host), p.Port, conninfoValue(p.User),
		conninfoValue(p.Password), conninfoValue(p.Database),
		conninfoValue(p.SSLMode),
	)
}

// conninfoValue quotes a libpq key/value conninfo value so spaces, quotes
// and embedded "key=value" text cannot break or inject parameters
// (G5-B22). libpq escapes backslash and single quote with a backslash.
func conninfoValue(value string) string {
	escaped := strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(value)
	return "'" + escaped + "'"
}

// Load reads config from YAML file, then overlays env vars, then CLI flags.
// Precedence: CLI > env > YAML > defaults. Persisted UI overrides replay on
// top of the result, except that trust ceilings take the most restrictive
// of an explicit file value and the override (config-03).
func Load(args []string) (*Config, error) {
	cfg := newDefaults()
	// Mode starts unset so inferMode can tell an explicit choice (YAML,
	// SAGE_MODE, --mode) from the built-in default.
	cfg.Mode = ""

	// Parse CLI flags to get config path and mode early.
	fs := flag.NewFlagSet("pg_sage_sidecar", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to config.yaml")
	mode := fs.String("mode", "", "Operating mode: standalone, fleet, or meta")
	pgHost := fs.String("pg-host", "", "PostgreSQL host")
	pgPort := fs.Int("pg-port", 0, "PostgreSQL port")
	pgUser := fs.String("pg-user", "", "PostgreSQL user")
	pgPassword := fs.String("pg-password", "", "PostgreSQL password")
	pgDatabase := fs.String("pg-database", "", "PostgreSQL database")
	pgSSLMode := fs.String("pg-sslmode", "", "PostgreSQL sslmode")
	pgURL := fs.String("pg-url", "", "PostgreSQL connection URL (overrides individual pg-* flags)")
	promAddr := fs.String("prom-addr", "", "Prometheus listen address")
	metaDB := fs.String("meta-db", "", "Metadata database connection string")
	encryptionKey := fs.String("encryption-key", "", "Passphrase for credential encryption")
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("parse flags: %w", err)
	}

	// Step 1: Load YAML (if provided or auto-detected).
	yamlPath := *configPath
	if yamlPath == "" {
		yamlPath = envOr("SAGE_CONFIG_PATH", "")
	}
	if yamlPath == "" {
		// Auto-detect config.yaml in CWD.
		if _, err := os.Stat("config.yaml"); err == nil {
			yamlPath = "config.yaml"
		}
	}
	if yamlPath != "" {
		if err := loadYAML(yamlPath, cfg); err != nil {
			return nil, fmt.Errorf("loading config %s: %w", yamlPath, err)
		}
		cfg.ConfigPath = yamlPath
	}

	// Step 2: Overlay environment variables, then NAME_FILE secrets.
	if err := overlayEnv(cfg); err != nil {
		return nil, err
	}
	if err := applySecretFiles(cfg); err != nil {
		return nil, err
	}
	markEnvPaths(cfg)

	// Step 3: Overlay CLI flags (highest precedence).
	if *mode != "" {
		cfg.Mode = *mode
	}
	if *pgHost != "" {
		cfg.Postgres.Host = *pgHost
	}
	if *pgPort != 0 {
		cfg.Postgres.Port = *pgPort
	}
	if *pgUser != "" {
		cfg.Postgres.User = *pgUser
	}
	if *pgPassword != "" {
		cfg.Postgres.Password = *pgPassword
	}
	if *pgDatabase != "" {
		cfg.Postgres.Database = *pgDatabase
	}
	if *pgSSLMode != "" {
		cfg.Postgres.SSLMode = *pgSSLMode
	}
	if *pgURL != "" {
		cfg.Postgres.DatabaseURL = *pgURL
	}
	if *promAddr != "" {
		cfg.Prometheus.ListenAddr = *promAddr
	}
	if *metaDB != "" {
		cfg.MetaDB = *metaDB
	}
	if *encryptionKey != "" {
		cfg.EncryptionKey = *encryptionKey
	}
	markFlagPaths(cfg, fs)

	// Legacy env-var compat.
	if v := os.Getenv("SAGE_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	cfg.TLSCert = os.Getenv("SAGE_TLS_CERT")
	cfg.TLSKey = os.Getenv("SAGE_TLS_KEY")

	if cfg.Mode == "" {
		cfg.Mode = inferMode(cfg)
	}

	// Normalize fleet/standalone config before validation.
	cfg.normalize()

	if err := validateMode(cfg); err != nil {
		return nil, err
	}
	if err := cfg.validateFileConfig(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}
	cfg.Safety.warnInheritedHostCPUCeiling()

	return cfg, nil
}

// validatePostgresPort checks the port standalone mode connects with; fleet
// and meta-db modes take their connections from elsewhere.
func (c *Config) validatePostgresPort() error {
	if c.Mode != "standalone" || c.MetaDB != "" {
		return nil
	}
	if p := c.Postgres.Port; p < 1 || p > 65535 {
		return fmt.Errorf("postgres.port must be from 1 to 65535, got %d (standalone "+
			"mode connects with the postgres settings; --meta-db selects a meta-db fleet)", p)
	}
	return nil
}

func (c *Config) validate() error {
	if err := c.validateTLS(); err != nil {
		return err
	}
	if err := c.API.validateTrustedOrigins(); err != nil {
		return err
	}
	if err := c.validateAgentNative(); err != nil {
		return err
	}
	if err := c.validateAuthz(); err != nil {
		return err
	}
	if err := c.validatePostgresPort(); err != nil {
		return err
	}
	if err := c.validateActionIdentity(); err != nil {
		return err
	}
	if c.Collector.IntervalSeconds <= 0 {
		return fmt.Errorf("collector.interval_seconds must be positive")
	}
	if c.Collector.BatchSize <= 0 {
		return fmt.Errorf("collector.batch_size must be positive")
	}
	if c.Collector.MaxQueries <= 0 {
		return fmt.Errorf("collector.max_queries must be positive")
	}
	if c.Analyzer.IntervalSeconds <= 0 {
		return fmt.Errorf("analyzer.interval_seconds must be positive")
	}
	if c.Analyzer.SlowQueryThresholdMs < 0 {
		return fmt.Errorf("analyzer.slow_query_threshold_ms must be non-negative")
	}
	if c.Analyzer.SlowQueryMinCalls < 0 {
		return fmt.Errorf("analyzer.slow_query_min_calls must be non-negative, got %d",
			c.Analyzer.SlowQueryMinCalls)
	}
	if err := c.validateAPITypedRanges(); err != nil {
		return err
	}
	if err := c.Analyzer.validateSchemaGuard(); err != nil {
		return err
	}
	if err := c.Analyzer.validateTableBloat(); err != nil {
		return err
	}
	if err := c.Analyzer.validateIndexBloat(); err != nil {
		return err
	}
	if err := validateBudgets(c.Budgets); err != nil {
		return err
	}
	if err := c.Briefing.validate(); err != nil {
		return err
	}
	if err := c.RCA.validate(); err != nil {
		return err
	}
	if err := c.SRE.validate(); err != nil {
		return err
	}
	if err := c.Ask.validate(); err != nil {
		return err
	}
	if err := c.Forecaster.validate(); err != nil {
		return err
	}
	if err := c.Facts.validate(); err != nil {
		return err
	}
	if err := c.validateRulePacks(); err != nil {
		return err
	}
	if err := c.Specialist.validate(); err != nil {
		return err
	}
	if err := c.LLM.validateWire(); err != nil {
		return err
	}
	if err := c.validateTrust(); err != nil {
		return err
	}
	if err := c.validateMigrationRehearsal(); err != nil {
		return err
	}
	if err := c.validateElevation(); err != nil {
		return err
	}
	if c.Safety.CPUCeilingPct <= 0 || c.Safety.CPUCeilingPct > 100 {
		return fmt.Errorf("safety.cpu_ceiling_pct must be 1-100")
	}
	if c.Safety.QueryTimeoutMs <= 0 {
		return fmt.Errorf("safety.query_timeout_ms must be positive")
	}
	if err := c.Safety.validateClassTimeouts(); err != nil {
		return err
	}
	if err := c.validateIOAdmission(); err != nil {
		return err
	}
	if err := c.LakebaseObservability.resolveAndValidate(os.Getenv); err != nil {
		return err
	}
	if err := c.validateCloudTelemetry(); err != nil {
		return err
	}
	if err := c.validateCost(); err != nil {
		return err
	}
	if err := c.Retention.validateSageFootprint(); err != nil {
		return err
	}
	if err := c.Analyzer.validateInactiveSlotMinMinutes(); err != nil {
		return err
	}
	if err := c.Analyzer.validateReplicationMinutes(); err != nil {
		return err
	}
	if err := c.Replication.validate(); err != nil {
		return err
	}
	if err := c.Analyzer.validateWraparoundFreezeShare(); err != nil {
		return err
	}
	if err := c.Analyzer.validateSelfCostBudget(); err != nil {
		return err
	}
	if err := c.SelfBudget.validate(); err != nil {
		return err
	}
	if err := c.Baseline.validate(); err != nil {
		return err
	}
	if err := c.validateHistory(); err != nil {
		return err
	}
	if err := c.LLM.Optimizer.RejectionMemory.validate(); err != nil {
		return err
	}
	if err := c.Tuning.validate(); err != nil {
		return err
	}
	if err := c.Tuner.validateParamShapeMinVariants(); err != nil {
		return err
	}
	if err := c.Tuner.validateHintCostTolerancePct(); err != nil {
		return err
	}
	if err := c.Retention.validateDecisionsDays(); err != nil {
		return err
	}
	if err := c.Retention.validateAuthAuditDays(); err != nil {
		return err
	}
	if err := c.Retention.validateStorage(); err != nil {
		return err
	}
	if err := c.SelfConfig.validate(); err != nil {
		return err
	}
	if err := c.validateFleetLearning(); err != nil {
		return err
	}
	if err := c.Fleet.Discovery.Validate(); err != nil {
		return err
	}
	if err := c.HA.validate(); err != nil {
		return err
	}
	if err := c.Backup.validate(); err != nil {
		return err
	}
	if err := c.Agents.validate(); err != nil {
		return err
	}
	if err := c.Audit.validate(); err != nil {
		return err
	}
	if err := c.MCP.OAuth.validate(c.MCP.Transport); err != nil {
		return err
	}
	if err := validateReplicas(c.Databases); err != nil {
		return err
	}
	if err := c.validateSLOOwners(); err != nil {
		return err
	}
	if err := c.Prometheus.validate(); err != nil {
		return err
	}

	// Fleet-specific validation.
	if c.Mode == "fleet" {
		if len(c.Databases) == 0 && !c.HasMetaDB() {
			return fmt.Errorf("fleet mode requires at least one database")
		}
		seen := make(map[string]bool, len(c.Databases))
		for i, db := range c.Databases {
			if db.Name == "" {
				return fmt.Errorf("databases[%d]: name must not be empty", i)
			}
			if db.Host == "" {
				return fmt.Errorf("databases[%d] %q: host must not be empty", i, db.Name)
			}
			if seen[db.Name] {
				return fmt.Errorf("databases[%d]: duplicate name %q", i, db.Name)
			}
			seen[db.Name] = true
		}
	}

	return nil
}

// validateTrust checks the trust level and the maintenance window grammar
// so a typo fails at load instead of silently meaning "never".
func (c *Config) validateTrust() error {
	if !IsTrustLevel(c.Trust.Level) {
		return fmt.Errorf(
			"trust.level must be observation, advisory, or autonomous (got %q)",
			c.Trust.Level,
		)
	}
	if err := c.Trust.validateCeiling(); err != nil {
		return err
	}
	return ValidateMaintenanceWindow(c.Trust.MaintenanceWindow)
}

func newDefaults() *Config {
	return &Config{
		Mode: DefaultMode,
		Postgres: PostgresConfig{
			Host:           "localhost",
			Port:           DefaultPGPort,
			User:           DefaultPGUser,
			Database:       DefaultPGDatabase,
			SSLMode:        DefaultPGSSLMode,
			MaxConnections: DefaultPGMaxConnections,
		},
		Collector: CollectorConfig{
			IntervalSeconds: int(DefaultCollectorInterval / time.Second),
			BatchSize:       DefaultCollectorBatchSize,
			MaxQueries:      DefaultCollectorMaxQueries,
		},
		Analyzer: AnalyzerConfig{
			IntervalSeconds:              int(DefaultAnalyzerInterval / time.Second),
			SlowQueryThresholdMs:         DefaultSlowQueryThresholdMs,
			SlowQueryMinCalls:            DefaultSlowQueryMinCalls,
			SeqScanMinRows:               DefaultSeqScanMinRows,
			UnusedIndexWindowDays:        DefaultUnusedIndexWindowDays,
			IndexBloatThresholdPct:       DefaultIndexBloatThresholdPct,
			IndexBloatMinBytes:           DefaultIndexBloatMinBytes,
			TableBloatDeadTuplePct:       DefaultTableBloatDeadTuplePct,
			TableBloatMinRows:            DefaultTableBloatMinRows,
			TableBloatMinBytes:           DefaultTableBloatMinBytes,
			AutovacuumTuneMinRows:        DefaultAutovacuumTuneMinRows,
			AnalyzeStaleMinRows:          DefaultAnalyzeStaleMinRows,
			AnalyzeStaleDays:             DefaultAnalyzeStaleDays,
			WraparoundFreezeShare:        DefaultWraparoundFreezeShare,
			IdleInTxTimeoutMinutes:       DefaultIdleInTxTimeoutMinutes,
			CacheHitRatioWarning:         DefaultCacheHitRatioWarning,
			XIDWraparoundWarning:         DefaultXIDWraparoundWarning,
			XIDWraparoundCritical:        DefaultXIDWraparoundCritical,
			RegressionThresholdPct:       DefaultRegressionThresholdPct,
			RegressionLookbackDays:       DefaultRegressionLookbackDays,
			CheckpointFreqWarningPerHour: DefaultCheckpointFreqWarningPerHour,
			WorkMemPromotionThreshold:    DefaultAnalyzerWorkMemPromotionThreshold,

			SchemaGuardDDLDebounceSeconds: DefaultSchemaGuardDDLDebounceSeconds,
			SelfCostBudgetMs:              DefaultSelfCostBudgetMs,
			InactiveSlotMinMinutes:        DefaultInactiveSlotMinMinutes,
			SubscriptionLagMinutes:        DefaultSubscriptionLagMinutes,
			CDCStallMinutes:               DefaultCDCStallMinutes,
			LockChain: LockChainConfig{
				Enabled:                  true,
				MinBlockedThreshold:      DefaultLockChainMinBlocked,
				CriticalBlockedThreshold: DefaultLockChainCriticalBlocked,
				IdleInTxTerminateMinutes: DefaultLockChainIdleInTxTerminate,
				ActiveQueryCancelMinutes: DefaultLockChainActiveQueryCancel,
				SafePatterns:             []string{"pg_sage", "replication", "patroni"},
			},
		},
		Safety: SafetyConfig{
			CPUCeilingPct:            DefaultCPUCeilingPct,
			DataIOCeilingPct:         DefaultDataIOCeilingPct,
			WALIOCeilingPct:          DefaultWALIOCeilingPct,
			QueryTimeoutMs:           DefaultQueryTimeoutMs,
			DDLTimeoutSeconds:        DefaultDDLTimeoutSeconds,
			DiskPressureThresholdPct: DefaultDiskPressureThresholdPct,
			BackoffConsecutiveSkips:  DefaultBackoffConsecutiveSkips,
			DormantIntervalSeconds:   DefaultDormantIntervalSeconds,
			LockTimeoutMs:            DefaultLockTimeoutMs,
			IndexBuildTimeoutSeconds: DefaultClassTimeoutSeconds,
			ReindexTimeoutSeconds:    DefaultClassTimeoutSeconds,
			VacuumTimeoutSeconds:     DefaultClassTimeoutSeconds,
			VacuumStallMinutes:       DefaultVacuumStallMinutes,
		},
		Trust: TrustConfig{
			Level:                 DefaultTrustLevel,
			Tier3Safe:             DefaultTier3Safe,
			Tier3Moderate:         DefaultTier3Moderate,
			Tier3HighRisk:         DefaultTier3HighRisk,
			RampSafeHours:         DefaultRampSafeHours,
			RampModerateHours:     DefaultRampModerateHours,
			RollbackThresholdPct:  DefaultRollbackThresholdPct,
			RollbackWindowMinutes: DefaultRollbackWindowMinutes,
			RollbackCooldownDays:  DefaultRollbackCooldownDays,
			CascadeCooldownCycles: DefaultCascadeCooldownCycles,
		},
		LLM: LLMConfig{
			Enabled:               DefaultLLMEnabled,
			TimeoutSeconds:        DefaultLLMTimeoutSeconds,
			TokenBudgetDaily:      DefaultLLMTokenBudget,
			ContextBudgetTokens:   DefaultLLMContextBudget,
			CooldownSeconds:       DefaultLLMCooldownSeconds,
			TokenParameter:        DefaultLLMTokenParameter,
			ToolReasoningEffort:   DefaultLLMToolReasoningEffort,
			RedactNumericLiterals: DefaultLLMRedactNumericLiterals,
			IndexOptimizer: IndexOptimizerConfig{
				Enabled:            DefaultIdxOptEnabled,
				MinQueryCalls:      DefaultIdxOptMinQueryCalls,
				MaxIndexesPerTable: DefaultIdxOptMaxIndexesPerTable,
				MaxIncludeColumns:  DefaultIdxOptMaxIncludeColumns,
				WriteHeavyRatio:    DefaultIdxOptWriteHeavyRatio,
			},
			Optimizer: OptimizerConfig{
				Enabled:              DefaultOptEnabled,
				MinQueryCalls:        DefaultOptMinQueryCalls,
				MaxIndexesPerTable:   DefaultOptMaxIndexesPerTable,
				MaxNewPerTable:       DefaultOptMaxNewPerTable,
				MaxIncludeColumns:    DefaultOptMaxIncludeColumns,
				WriteHeavyRatioPct:   DefaultOptWriteHeavyRatioPct,
				MinSnapshots:         DefaultOptMinSnapshots,
				HypoPGMinImprovePct:  DefaultOptHypoPGMinImprovePct,
				ConfidenceThreshold:  DefaultOptConfidenceThreshold,
				WriteImpactThreshPct: DefaultOptWriteImpactThreshPct,
				RejectionMemory:      DefaultOptimizerRejectionMemory(),
			},
			OptimizerLLM: OptimizerLLMConfig{
				Enabled:           DefaultOptLLMEnabled,
				TimeoutSeconds:    DefaultOptLLMTimeoutSeconds,
				TokenBudgetDaily:  DefaultOptLLMTokenBudget,
				CooldownSeconds:   DefaultOptLLMCooldownSeconds,
				MaxOutputTokens:   DefaultOptLLMMaxOutputTokens,
				FallbackToGeneral: true,
			},
		},
		Advisor: AdvisorConfig{
			Enabled:           DefaultAdvisorEnabled,
			IntervalSeconds:   DefaultAdvisorIntervalSeconds,
			VacuumEnabled:     true,
			WALEnabled:        true,
			ConnectionEnabled: true,
			MemoryEnabled:     true,
			BudgetProposals:   true,
			RewriteEnabled:    true,
			BloatEnabled:      true,
		},
		Briefing: BriefingConfig{
			Schedule:  DefaultBriefingSchedule,
			Channels:  []string{"stdout"},
			FleetMode: BriefingFleetRollup,
		},
		Alerting: AlertingConfig{
			Enabled:              false,
			CheckIntervalSeconds: DefaultAlertingCheckInterval,
			CooldownMinutes:      DefaultAlertingCooldown,
			RepeatMinutes:        ResolveAlertRepeatMinutes(nil),
			Timezone:             "UTC",
		},
		SchemaLint: SchemaLintConfig{
			ScanIntervalMinutes: DefaultSchemaLintScanIntervalMinutes,
		},
		Migration: MigrationConfig{
			PollIntervalSeconds:  DefaultMigrationPollIntervalSeconds,
			ContractDelayMinutes: DefaultMigrationContractDelayMinutes,
			MaxLockMS:            DefaultMigrationMaxLockMS,
		},
		Rehearsal: RehearsalConfig{PlanFlipPct: DefaultRehearsalPlanFlipPct},
		AutoExplain: AutoExplainConfig{
			Enabled:                true,
			LogMinDurationMs:       DefaultAutoExplainLogMinDuration,
			CollectIntervalSeconds: DefaultAutoExplainCollectInterval,
			MaxPlansPerCycle:       DefaultAutoExplainMaxPlansPerCycle,
		},
		Forecaster: ForecasterConfig{
			Enabled:                    true,
			LookbackDays:               DefaultForecasterLookbackDays,
			DiskWarnGrowthGBDay:        DefaultForecasterDiskWarnGBDay,
			ConnectionWarnPct:          DefaultForecasterConnectionPct,
			CacheWarnThreshold:         DefaultForecasterCacheThreshold,
			CacheMinBlocksPerDay:       DefaultForecasterCacheMinBlocksPerDay,
			CheckpointRequestedPerHour: DefaultForecasterCheckpointRequestedPerHour,
			DiskCapacityBytes:          0,
			MinRSquared:                DefaultForecasterMinRSquared,
		},
		// LogWatch defaults were previously absent here, so with
		// logwatch enabled but no explicit values, MaxLinesPerCycle and
		// TempFileMinBytes defaulted to 0 — unbounded parsing and a
		// defeated 10MB temp-file floor (W1, default-value masking).
		LogWatch: LogWatchConfig{
			PollIntervalMs:   DefaultLogWatchPollIntervalMs,
			DedupWindowS:     DefaultLogWatchDedupWindowS,
			MaxLineLenBytes:  DefaultLogWatchMaxLineLenBytes,
			TempFileMinBytes: DefaultLogWatchTempFileMinBytes,
			MaxLinesPerCycle: DefaultLogWatchMaxLinesPerCycle,
		},
		Tuner: TunerConfig{
			Enabled:                true,
			WorkMemMaxMB:           DefaultTunerWorkMemMaxMB,
			PlanTimeRatio:          DefaultTunerPlanTimeRatio,
			NestedLoopRowThreshold: DefaultTunerNestedLoopRowThresh,
			ParallelMinTableRows:   DefaultTunerParallelMinRows,
			MinQueryCalls:          DefaultTunerMinQueryCalls,
			LLMEnabled:             DefaultTunerLLMEnabled,
			VerifyAfterApply:       true,

			// Feature 1 — Hint revalidation loop.
			HintRetirementDays:        DefaultTunerHintRetirementDays,
			RevalidationIntervalHours: DefaultTunerRevalidationIntervalHours,
			HintAbsentCycles:          DefaultTunerHintAbsentCycles,
			PlanMaxAgeMinutes:         DefaultTunerPlanMaxAgeMinutes,
			ParamShapeMinVariants:     DefaultTunerParamShapeMinVariants,
			HintCostTolerancePct:      DefaultTunerHintCostTolerancePct,

			// Feature 2 — Stale-stats detection + ANALYZE.
			StaleStatsEstimateSkew: DefaultTunerStaleStatsEstimateSkew,
			StaleStatsModRatio:     DefaultTunerStaleStatsModRatio,
			StaleStatsAgeMinutes:   DefaultTunerStaleStatsAgeMinutes,
			AnalyzeMaxTableMB:      DefaultTunerAnalyzeMaxTableMB,
			AnalyzeCooldownMinutes: DefaultTunerAnalyzeCooldownMinutes,
			AnalyzeTimeoutMs:       DefaultTunerAnalyzeTimeoutMs,
			MaxConcurrentAnalyze:   DefaultTunerMaxConcurrentAnalyze,
		},
		Tuning: DefaultTuning(),
		RCA: RCAConfig{
			Enabled:                    true,
			LLMCorrelationThreshold:    DefaultRCALLMCorrelationThreshold,
			DedupWindowMinutes:         DefaultRCADedupWindowMinutes,
			EscalationCycles:           DefaultRCAEscalationCycles,
			ResolutionCycles:           DefaultRCAResolutionCycles,
			StaleAfterHours:            DefaultRCAStaleAfterHours,
			VacuumMinDeadTuples:        DefaultRCAVacuumMinDeadTuples,
			VacuumMinTableMB:           DefaultRCAVacuumMinTableMB,
			XminHolderMinMinutes:       DefaultRCAXminHolderMinMinutes,
			PreparedXactMinAgeMinutes:  DefaultRCAPreparedXactMinAgeMinutes,
			ConnectionSaturationPct:    DefaultRCAConnectionSaturationPct,
			ReplicationLagThresholdS:   DefaultRCAReplicationLagThresholdS,
			WALSpikeMultiplier:         DefaultRCAWALSpikeMultiplier,
			LockChainIntervalSeconds:   DefaultRCALockChainIntervalSeconds,
			NarrationEnabled:           DefaultRCANarrationEnabled,
			LogFastPathIntervalSeconds: DefaultRCALogFastPathIntervalSeconds,
			RecurrenceRenotifyHours:    DefaultRCARecurrenceRenotifyHours,
		},
		SRE:        defaultSREConfig(),
		Specialist: defaultSpecialistConfig(),
		Runaway: RunawayConfig{
			Enabled: false,
			Policies: []RunawayPolicy{
				{Name: "long_running", MaxDurationMinutes: 30, WarnCycles: 2, CancelCycles: 4},
				{Name: "blocker", MaxBlockedSessions: 5, WarnCycles: 1, CancelCycles: 2},
			},
			SafePatterns: []string{"pg_dump", "pg_basebackup"},
		},
		Explain: ExplainConfig{
			Enabled:         true,
			TimeoutMs:       DefaultExplainTimeoutMs,
			CacheTTLMinutes: DefaultExplainCacheTTLMinutes,
			MaxTokens:       DefaultExplainMaxTokens,
		},
		Retention: RetentionConfig{
			SnapshotsDays: DefaultRetentionSnapshotsDays,
			FindingsDays:  DefaultRetentionFindingsDays,
			ActionsDays:   DefaultRetentionActionsDays,
			ExplainsDays:  DefaultRetentionExplainsDays,

			SageSizeWarningPct: DefaultRetentionSageSizeWarningPct,
			DecisionsDays:      DefaultRetentionDecisionsDays,
			AuthAuditDays:      DefaultRetentionAuthAuditDays,
			QueryStoreDays:     DefaultRetentionQueryStoreDays,
			SnapshotsMaxPct:    DefaultRetentionSnapshotsMaxPct,
		},
		NotificationPolicy: NotificationPolicyConfig{
			AllowPrivateTargets: DefaultNotificationPolicyAllowPrivateTargets,
		},
		Prometheus: PrometheusConfig{
			ListenAddr: DefaultPrometheusListenAddr,
		},
		API: APIConfig{
			ListenAddr: DefaultAPIListenAddr,
		},
		Policy: PolicyConfig{Profile: DefaultPolicyProfile},
		Value:  ValueConfig{ToilModelVersion: DefaultToilModelVersion},
		Verify: VerifyConfig{
			WindowMinutes:    DefaultVerifyWindowMinutes,
			WindowMaxMinutes: DefaultVerifyWindowMaxMinutes,
			MinGainPct:       DefaultVerifyMinGainPct,
			RegressPct:       DefaultVerifyRegressPct,
			WriteImpactPct:   DefaultVerifyWriteImpactPct,
			MinSamples:       DefaultVerifyMinSamples,
			DropWindowHours:  DefaultVerifyDropWindowHours,
			IOBaselineDays:   DefaultIOBaselineDays,
			IOSampleDays:     DefaultIOSampleRetentionDays,
		},
		Clone: CloneProviderConfig{
			Provider:           DefaultCloneProvider,
			MaxCloneAgeMinutes: DefaultCloneMaxAgeMinutes,
			Lakebase: CloneLakebaseConfig{TTLMinutes: DefaultCloneLakebaseTTLMinutes,
				EndpointMaxCU: DefaultCloneLakebaseEndpointMaxCU},
		},
		Custodian: CustodianConfig{
			Freeze: FreezeCustodianConfig{RedBufferPct: DefaultFreezeRedBufferPct},
			WAL: WALCustodianConfig{
				AbandonAfterMinutes:       DefaultWALAbandonAfterMinutes,
				RetainedWALDiskPctCeiling: DefaultWALRetainedDiskPctCeiling,
			},
		},
		MCP: MCPConfig{Enabled: DefaultMCPEnabled, Transport: DefaultMCPTransport,
			OAuth:              defaultMCPOAuthConfig(),
			CallTimeoutSeconds: DefaultMCPCallTimeoutSeconds},
		Ask:        defaultAskConfig(),
		Facts:      defaultFactsConfig(),
		SelfBudget: DefaultSelfBudget(),
		Baseline:   DefaultBaseline(),
		History:    DefaultHistory(),
		OAuth: OAuthConfig{
			DefaultRole:   "viewer",
			GroupsClaim:   "groups",
			UnmappedUsers: UnmappedUsersDeny,
		},
		SelfConfig:    defaultSelfConfigConfig(),
		FleetLearning: defaultFleetLearningConfig(),
		Fleet:         defaultFleetConfig(),
		HA:            defaultHAConfig(),
		Backup:        defaultBackupConfig(),
		Agents:        defaultAgentsConfig(),
		Audit:         defaultAuditConfig(),
	}
}

func loadYAML(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Expand ${ENV_VAR} references with validation. Only the braced
	// form is expanded; a bare '$' (e.g. in a password) is left intact.
	raw := string(data)
	if err := checkBracedSecretFiles(raw); err != nil {
		return err
	}
	expanded := expandBracedEnv(raw)

	// Warn about env vars that expanded to empty strings. This catches the
	// common case where ${SAGE_LLM_API_KEY} is in the YAML but the env var
	// is not set, leaving an empty value that silently breaks the feature.
	for _, warning := range unexpandedEnvWarnings(path, raw) {
		_, _ = fmt.Fprintln(configWarningOutput, warning)
	}
	if err := requireOneDocument(expanded); err != nil {
		return err
	}
	if err := rejectRetiredTopLevelConfig(expanded); err != nil {
		return err
	}
	expanded, err = dropRetired(expanded)
	if err != nil {
		return err
	}

	candidate := Clone(cfg)
	decoder := yaml.NewDecoder(strings.NewReader(expanded))
	decoder.KnownFields(true)
	if err := decoder.Decode(candidate); err != nil {
		return err
	}
	if err := migrateLegacyIndexOptimizer(expanded, candidate); err != nil {
		return err
	}
	if candidate.explicitPaths, err = yamlPresencePaths(expanded); err != nil {
		return err
	}
	*cfg = *candidate
	return nil
}

func rejectRetiredTopLevelConfig(raw string) error {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		return err
	}
	if len(document.Content) == 0 ||
		len(document.Content[0].Content) == 0 {
		return nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		switch root.Content[i].Value {
		case "notifications":
			return fmt.Errorf(
				"configuration key %q is retired; use %q instead",
				"notifications", "alerting",
			)
		}
	}
	return nil
}

// configWarningOutput receives configuration warnings (stderr in
// production; tests capture it).
var configWarningOutput io.Writer = os.Stderr

// unexpandedEnvWarnings lists ${VAR} references in YAML scalar values whose
// variable is unset. Comments are not configuration and are ignored.
func unexpandedEnvWarnings(path, raw string) []string {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		return nil // the strict decoder reports the syntax error
	}
	var warnings []string
	seen := map[string]bool{}
	walkScalars(&document, func(value string) {
		for _, name := range bracedEnvNames(value) {
			if seen[name] || os.Getenv(name) != "" ||
				os.Getenv(name+secretFileSuffix) != "" {
				continue
			}
			seen[name] = true
			warnings = append(warnings, fmt.Sprintf(
				"WARNING: config %q references ${%s} but it is not set "+
					"in the environment", path, name))
		}
	})
	return warnings
}

func walkScalars(node *yaml.Node, visit func(string)) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode {
		visit(node.Value)
	}
	for _, child := range node.Content {
		walkScalars(child, visit)
	}
}

func bracedEnvNames(value string) []string {
	var names []string
	for _, match := range braceEnvRe.FindAllStringSubmatch(value, -1) {
		names = append(names, match[1])
	}
	return names
}

// inferMode picks the mode when none was configured. A meta database means
// a meta-db fleet (never standalone: standalone+meta registers a phantom
// instance); anything else monitors one database standalone (G10-B01).
func inferMode(cfg *Config) string {
	if cfg.MetaDB != "" {
		return ModeMeta
	}
	return DefaultMode
}

// validateMode rejects unknown modes, meta mode without a meta database,
// standalone mode with one (main would run the meta-db fleet while metrics
// report standalone), and the removed extension mode, which served the API
// over the deleted C extension's schema.
func validateMode(cfg *Config) error {
	switch cfg.Mode {
	case "standalone":
		if cfg.MetaDB != "" {
			return errors.New("mode standalone cannot be combined with " +
				"meta_db; drop mode (meta is inferred) or set mode: meta")
		}
		return nil
	case "fleet":
		return nil
	case ModeMeta:
		if cfg.MetaDB == "" {
			return fmt.Errorf("mode %q requires --meta-db (meta_db)", ModeMeta)
		}
		return nil
	case "extension":
		return fmt.Errorf("mode %q was removed with the C extension: use "+
			"standalone for one database, fleet for several, or --meta-db "+
			"for a meta-db fleet", cfg.Mode)
	default:
		return fmt.Errorf("invalid mode %q: must be 'standalone', 'fleet', "+
			"or 'meta'", cfg.Mode)
	}
}

// IsStandalone returns true if running in standalone mode.
func (c *Config) IsStandalone() bool {
	return c.Mode == "standalone"
}

// IsFleet returns true if running in fleet mode.
func (c *Config) IsFleet() bool {
	return c.Mode == "fleet"
}

// HasMetaDB returns true if a metadata database is configured.
func (c *Config) HasMetaDB() bool {
	return c.MetaDB != ""
}

// HasEncryptionKey returns true if an encryption passphrase is set.
func (c *Config) HasEncryptionKey() bool {
	return c.EncryptionKey != ""
}

// DefaultConfig returns a Config populated with defaults.
// Exported for use by cmd/gen_config_meta (v0.8.5 Feature 5).
func DefaultConfig() *Config {
	return newDefaults()
}
