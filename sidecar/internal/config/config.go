package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for pg_sage standalone sidecar.
type Config struct {
	Mode string `yaml:"mode"` // "extension" or "standalone"

	Postgres   PostgresConfig   `yaml:"postgres"`
	Collector  CollectorConfig  `yaml:"collector"`
	Analyzer   AnalyzerConfig   `yaml:"analyzer"`
	Safety     SafetyConfig     `yaml:"safety"`
	Trust      TrustConfig      `yaml:"trust"`
	LLM        LLMConfig        `yaml:"llm"`
	Advisor    AdvisorConfig    `yaml:"advisor"`
	Briefing   BriefingConfig   `yaml:"briefing"`
	Retention  RetentionConfig  `yaml:"retention"`
	MCP        MCPConfig        `yaml:"mcp"`
	Prometheus PrometheusConfig `yaml:"prometheus"`

	// Legacy env-var fields (extension mode compat)
	APIKey  string `yaml:"-"`
	TLSCert string `yaml:"-"`
	TLSKey  string `yaml:"-"`

	// Runtime (not from config file)
	ConfigPath        string `yaml:"-"`
	PGVersionNum      int    `yaml:"-"` // e.g. 160000 for PG16
	HasWALColumns     bool   `yaml:"-"`
	HasPlanTimeColumns bool  `yaml:"-"`
}

type PostgresConfig struct {
	Host           string `yaml:"host"`
	Port           int    `yaml:"port"`
	User           string `yaml:"user"`
	Password       string `yaml:"password"`
	Database       string `yaml:"database"`
	SSLMode        string `yaml:"sslmode"`
	MaxConnections int    `yaml:"max_connections"`
	DatabaseURL    string `yaml:"database_url"` // overrides individual fields
}

type CollectorConfig struct {
	IntervalSeconds int `yaml:"interval_seconds"`
	BatchSize       int `yaml:"batch_size"`
}

type AnalyzerConfig struct {
	IntervalSeconds              int     `yaml:"interval_seconds"`
	SlowQueryThresholdMs         int     `yaml:"slow_query_threshold_ms"`
	SeqScanMinRows               int     `yaml:"seq_scan_min_rows"`
	UnusedIndexWindowDays        int     `yaml:"unused_index_window_days"`
	IndexBloatThresholdPct       int     `yaml:"index_bloat_threshold_pct"`
	TableBloatDeadTuplePct       int     `yaml:"table_bloat_dead_tuple_pct"`
	TableBloatMinRows            int     `yaml:"table_bloat_min_rows"`
	IdleInTxTimeoutMinutes       int     `yaml:"idle_in_transaction_timeout_minutes"`
	CacheHitRatioWarning         float64 `yaml:"cache_hit_ratio_warning"`
	XIDWraparoundWarning         int64   `yaml:"xid_wraparound_warning"`
	XIDWraparoundCritical        int64   `yaml:"xid_wraparound_critical"`
	RegressionThresholdPct       int     `yaml:"regression_threshold_pct"`
	RegressionLookbackDays       int     `yaml:"regression_lookback_days"`
	CheckpointFreqWarningPerHour int     `yaml:"checkpoint_frequency_warning_per_hour"`
}

type SafetyConfig struct {
	CPUCeilingPct            int `yaml:"cpu_ceiling_pct"`
	QueryTimeoutMs           int `yaml:"query_timeout_ms"`
	DDLTimeoutSeconds        int `yaml:"ddl_timeout_seconds"`
	DiskPressureThresholdPct int `yaml:"disk_pressure_threshold_pct"`
	BackoffConsecutiveSkips   int `yaml:"backoff_consecutive_skips"`
	DormantIntervalSeconds   int `yaml:"dormant_interval_seconds"`
}

type TrustConfig struct {
	Level                string `yaml:"level"` // observation, advisory, autonomous
	RampStart            string `yaml:"ramp_start"`
	MaintenanceWindow    string `yaml:"maintenance_window"`
	Tier3Safe            bool   `yaml:"tier3_safe"`
	Tier3Moderate        bool   `yaml:"tier3_moderate"`
	Tier3HighRisk        bool   `yaml:"tier3_high_risk"`
	RollbackThresholdPct int    `yaml:"rollback_threshold_pct"`
	RollbackWindowMinutes int   `yaml:"rollback_window_minutes"`
	RollbackCooldownDays int    `yaml:"rollback_cooldown_days"`
}

type LLMConfig struct {
	Enabled             bool               `yaml:"enabled"`
	Endpoint            string             `yaml:"endpoint"`
	APIKey              string             `yaml:"api_key"`
	Model               string             `yaml:"model"`
	TimeoutSeconds      int                `yaml:"timeout_seconds"`
	TokenBudgetDaily    int                `yaml:"token_budget_daily"`
	ContextBudgetTokens int                `yaml:"context_budget_tokens"`
	CooldownSeconds     int                `yaml:"cooldown_seconds"`
	IndexOptimizer      IndexOptimizerConfig `yaml:"index_optimizer"` // Deprecated: use Optimizer.
	Optimizer    OptimizerConfig    `yaml:"optimizer"`
	OptimizerLLM OptimizerLLMConfig `yaml:"optimizer_llm"`
}

type IndexOptimizerConfig struct {
	Enabled           bool `yaml:"enabled"`
	MinQueryCalls     int  `yaml:"min_query_calls"`
	MaxIndexesPerTable int `yaml:"max_indexes_per_table"`
	MaxIncludeColumns int  `yaml:"max_include_columns"`
	OverIndexedRatio  int  `yaml:"over_indexed_ratio_pct"`
	WriteHeavyRatio   int  `yaml:"write_heavy_ratio_pct"`
}

// OptimizerConfig controls the index optimizer v2 behavior.
type OptimizerConfig struct {
	Enabled              bool    `yaml:"enabled"`
	MinQueryCalls        int     `yaml:"min_query_calls"`
	MaxIndexesPerTable   int     `yaml:"max_indexes_per_table"`
	MaxNewPerTable       int     `yaml:"max_new_per_table"`
	MaxIncludeColumns    int     `yaml:"max_include_columns"`
	OverIndexedRatioPct  int     `yaml:"over_indexed_ratio_pct"`
	WriteHeavyRatioPct   int     `yaml:"write_heavy_ratio_pct"`
	MinSnapshots         int     `yaml:"min_snapshots"`
	HypoPGMinImprovePct  float64 `yaml:"hypopg_min_improvement_pct"`
	PlanSource           string  `yaml:"plan_source"`
	ConfidenceThreshold  float64 `yaml:"confidence_threshold"`
	WriteImpactThreshPct float64 `yaml:"write_impact_threshold_pct"`
}

// OptimizerLLMConfig configures the dedicated optimizer LLM (reasoning-tier).
type OptimizerLLMConfig struct {
	Enabled          bool   `yaml:"enabled"`
	Endpoint         string `yaml:"endpoint"`
	APIKey           string `yaml:"api_key"`
	Model            string `yaml:"model"`
	TimeoutSeconds   int    `yaml:"timeout_seconds"`
	TokenBudgetDaily int    `yaml:"token_budget_daily"`
	CooldownSeconds  int    `yaml:"cooldown_seconds"`
	MaxOutputTokens  int    `yaml:"max_output_tokens"`
	FallbackToGeneral bool  `yaml:"fallback_to_general"`
}

type AdvisorConfig struct {
	Enabled           bool `yaml:"enabled"`
	IntervalSeconds   int  `yaml:"interval_seconds"`
	VacuumEnabled     bool `yaml:"vacuum_enabled"`
	WALEnabled        bool `yaml:"wal_enabled"`
	ConnectionEnabled bool `yaml:"connection_enabled"`
	MemoryEnabled     bool `yaml:"memory_enabled"`
	RewriteEnabled    bool `yaml:"rewrite_enabled"`
	BloatEnabled      bool `yaml:"bloat_enabled"`
}

type BriefingConfig struct {
	Schedule        string   `yaml:"schedule"`
	Channels        []string `yaml:"channels"`
	SlackWebhookURL string   `yaml:"slack_webhook_url"`
}

type RetentionConfig struct {
	SnapshotsDays int `yaml:"snapshots_days"`
	FindingsDays  int `yaml:"findings_days"`
	ActionsDays   int `yaml:"actions_days"`
	ExplainsDays  int `yaml:"explains_days"`
}

type MCPConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ListenAddr string `yaml:"listen_addr"`
}

type PrometheusConfig struct {
	ListenAddr string `yaml:"listen_addr"`
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

func (c *SafetyConfig) QueryTimeout() time.Duration {
	return time.Duration(c.QueryTimeoutMs) * time.Millisecond
}

func (c *SafetyConfig) DDLTimeout() time.Duration {
	return time.Duration(c.DDLTimeoutSeconds) * time.Second
}

// DSN builds a libpq connection string from individual fields.
func (p *PostgresConfig) DSN() string {
	if p.DatabaseURL != "" {
		return p.DatabaseURL
	}
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		p.Host, p.Port, p.User, p.Password, p.Database, p.SSLMode,
	)
}

// Load reads config from YAML file, then overlays env vars, then CLI flags.
// Precedence: CLI > env > YAML > defaults.
func Load(args []string) (*Config, error) {
	cfg := newDefaults()

	// Parse CLI flags to get config path and mode early.
	fs := flag.NewFlagSet("pg_sage_sidecar", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to config.yaml")
	mode := fs.String("mode", "", "Operating mode: extension or standalone")
	pgHost := fs.String("pg-host", "", "PostgreSQL host")
	pgPort := fs.Int("pg-port", 0, "PostgreSQL port")
	pgUser := fs.String("pg-user", "", "PostgreSQL user")
	pgPassword := fs.String("pg-password", "", "PostgreSQL password")
	pgDatabase := fs.String("pg-database", "", "PostgreSQL database")
	pgSSLMode := fs.String("pg-sslmode", "", "PostgreSQL sslmode")
	pgURL := fs.String("pg-url", "", "PostgreSQL connection URL (overrides individual pg-* flags)")
	mcpAddr := fs.String("mcp-addr", "", "MCP listen address")
	promAddr := fs.String("prom-addr", "", "Prometheus listen address")
	_ = fs.Parse(args)

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

	// Step 2: Overlay environment variables.
	overlayEnv(cfg)

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
	if *mcpAddr != "" {
		cfg.MCP.ListenAddr = *mcpAddr
	}
	if *promAddr != "" {
		cfg.Prometheus.ListenAddr = *promAddr
	}

	// Legacy env-var compat.
	cfg.APIKey = os.Getenv("SAGE_API_KEY")
	cfg.TLSCert = os.Getenv("SAGE_TLS_CERT")
	cfg.TLSKey = os.Getenv("SAGE_TLS_KEY")

	// Validate.
	if cfg.Mode != "extension" && cfg.Mode != "standalone" {
		return nil, fmt.Errorf("invalid mode %q: must be 'extension' or 'standalone'", cfg.Mode)
	}
	if cfg.Mode == "standalone" && cfg.Postgres.DSN() == "" {
		return nil, fmt.Errorf("standalone mode requires postgres connection config")
	}

	// Migrate deprecated index_optimizer → optimizer if optimizer wasn't
	// explicitly set but the legacy key was.
	if !cfg.LLM.Optimizer.Enabled && cfg.LLM.IndexOptimizer.Enabled {
		cfg.LLM.Optimizer.Enabled = true
		if cfg.LLM.IndexOptimizer.MinQueryCalls > 0 {
			cfg.LLM.Optimizer.MinQueryCalls = cfg.LLM.IndexOptimizer.MinQueryCalls
		}
		if cfg.LLM.IndexOptimizer.MaxIndexesPerTable > 0 {
			cfg.LLM.Optimizer.MaxIndexesPerTable = cfg.LLM.IndexOptimizer.MaxIndexesPerTable
		}
		if cfg.LLM.IndexOptimizer.MaxIncludeColumns > 0 {
			cfg.LLM.Optimizer.MaxIncludeColumns = cfg.LLM.IndexOptimizer.MaxIncludeColumns
		}
		if cfg.LLM.IndexOptimizer.OverIndexedRatio > 0 {
			cfg.LLM.Optimizer.OverIndexedRatioPct = cfg.LLM.IndexOptimizer.OverIndexedRatio
		}
		if cfg.LLM.IndexOptimizer.WriteHeavyRatio > 0 {
			cfg.LLM.Optimizer.WriteHeavyRatioPct = cfg.LLM.IndexOptimizer.WriteHeavyRatio
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return cfg, nil
}

func (c *Config) validate() error {
	if c.Collector.IntervalSeconds <= 0 {
		return fmt.Errorf("collector.interval_seconds must be positive")
	}
	if c.Collector.BatchSize <= 0 {
		return fmt.Errorf("collector.batch_size must be positive")
	}
	if c.Analyzer.IntervalSeconds <= 0 {
		return fmt.Errorf("analyzer.interval_seconds must be positive")
	}
	if c.Analyzer.SlowQueryThresholdMs < 0 {
		return fmt.Errorf("analyzer.slow_query_threshold_ms must be non-negative")
	}
	validTrust := map[string]bool{
		"observation": true, "advisory": true, "autonomous": true,
	}
	if !validTrust[c.Trust.Level] {
		return fmt.Errorf(
			"trust.level must be observation, advisory, or autonomous (got %q)",
			c.Trust.Level,
		)
	}
	if c.Safety.CPUCeilingPct <= 0 || c.Safety.CPUCeilingPct > 100 {
		return fmt.Errorf("safety.cpu_ceiling_pct must be 1-100")
	}
	if c.Safety.QueryTimeoutMs <= 0 {
		return fmt.Errorf("safety.query_timeout_ms must be positive")
	}
	return nil
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
		},
		Analyzer: AnalyzerConfig{
			IntervalSeconds:              int(DefaultAnalyzerInterval / time.Second),
			SlowQueryThresholdMs:         DefaultSlowQueryThresholdMs,
			SeqScanMinRows:               DefaultSeqScanMinRows,
			UnusedIndexWindowDays:        DefaultUnusedIndexWindowDays,
			IndexBloatThresholdPct:       DefaultIndexBloatThresholdPct,
			TableBloatDeadTuplePct:       DefaultTableBloatDeadTuplePct,
			TableBloatMinRows:           DefaultTableBloatMinRows,
			IdleInTxTimeoutMinutes:       DefaultIdleInTxTimeoutMinutes,
			CacheHitRatioWarning:         DefaultCacheHitRatioWarning,
			XIDWraparoundWarning:         DefaultXIDWraparoundWarning,
			XIDWraparoundCritical:        DefaultXIDWraparoundCritical,
			RegressionThresholdPct:       DefaultRegressionThresholdPct,
			RegressionLookbackDays:       DefaultRegressionLookbackDays,
			CheckpointFreqWarningPerHour: DefaultCheckpointFreqWarningPerHour,
		},
		Safety: SafetyConfig{
			CPUCeilingPct:            DefaultCPUCeilingPct,
			QueryTimeoutMs:           DefaultQueryTimeoutMs,
			DDLTimeoutSeconds:        DefaultDDLTimeoutSeconds,
			DiskPressureThresholdPct: DefaultDiskPressureThresholdPct,
			BackoffConsecutiveSkips:  DefaultBackoffConsecutiveSkips,
			DormantIntervalSeconds:   DefaultDormantIntervalSeconds,
		},
		Trust: TrustConfig{
			Level:                 DefaultTrustLevel,
			Tier3Safe:             DefaultTier3Safe,
			Tier3Moderate:         DefaultTier3Moderate,
			Tier3HighRisk:         DefaultTier3HighRisk,
			RollbackThresholdPct:  DefaultRollbackThresholdPct,
			RollbackWindowMinutes: DefaultRollbackWindowMinutes,
			RollbackCooldownDays:  DefaultRollbackCooldownDays,
		},
		LLM: LLMConfig{
			Enabled:            DefaultLLMEnabled,
			TimeoutSeconds:     DefaultLLMTimeoutSeconds,
			TokenBudgetDaily:   DefaultLLMTokenBudget,
			ContextBudgetTokens: DefaultLLMContextBudget,
			CooldownSeconds:    DefaultLLMCooldownSeconds,
			IndexOptimizer: IndexOptimizerConfig{
				Enabled:            DefaultIdxOptEnabled,
				MinQueryCalls:      DefaultIdxOptMinQueryCalls,
				MaxIndexesPerTable: DefaultIdxOptMaxIndexesPerTable,
				MaxIncludeColumns:  DefaultIdxOptMaxIncludeColumns,
				OverIndexedRatio:   DefaultIdxOptOverIndexedRatio,
				WriteHeavyRatio:    DefaultIdxOptWriteHeavyRatio,
			},
			Optimizer: OptimizerConfig{
				Enabled:              DefaultOptEnabled,
				MinQueryCalls:        DefaultOptMinQueryCalls,
				MaxIndexesPerTable:   DefaultOptMaxIndexesPerTable,
				MaxNewPerTable:       DefaultOptMaxNewPerTable,
				MaxIncludeColumns:    DefaultOptMaxIncludeColumns,
				OverIndexedRatioPct:  DefaultOptOverIndexedRatioPct,
				WriteHeavyRatioPct:   DefaultOptWriteHeavyRatioPct,
				MinSnapshots:         DefaultOptMinSnapshots,
				HypoPGMinImprovePct:  DefaultOptHypoPGMinImprovePct,
				PlanSource:           DefaultOptPlanSource,
				ConfidenceThreshold:  DefaultOptConfidenceThreshold,
				WriteImpactThreshPct: DefaultOptWriteImpactThreshPct,
			},
			OptimizerLLM: OptimizerLLMConfig{
				Enabled:           false,
				TimeoutSeconds:    DefaultOptLLMTimeoutSeconds,
				TokenBudgetDaily:  DefaultOptLLMTokenBudget,
				CooldownSeconds:   DefaultOptLLMCooldownSeconds,
				MaxOutputTokens:   DefaultOptLLMMaxOutputTokens,
				FallbackToGeneral: true,
			},
		},
		Advisor: AdvisorConfig{
			Enabled:           false,
			IntervalSeconds:   86400,
			VacuumEnabled:     true,
			WALEnabled:        true,
			ConnectionEnabled: true,
			MemoryEnabled:     true,
			RewriteEnabled:    true,
			BloatEnabled:      true,
		},
		Briefing: BriefingConfig{
			Schedule: DefaultBriefingSchedule,
			Channels: []string{"stdout"},
		},
		Retention: RetentionConfig{
			SnapshotsDays: DefaultRetentionSnapshotsDays,
			FindingsDays:  DefaultRetentionFindingsDays,
			ActionsDays:   DefaultRetentionActionsDays,
			ExplainsDays:  DefaultRetentionExplainsDays,
		},
		MCP: MCPConfig{
			Enabled:    true,
			ListenAddr: DefaultMCPListenAddr,
		},
		Prometheus: PrometheusConfig{
			ListenAddr: DefaultPrometheusListenAddr,
		},
	}
}

func loadYAML(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Expand ${ENV_VAR} references with validation.
	raw := string(data)
	expanded := os.ExpandEnv(raw)

	// Warn about env vars that expanded to empty strings. This catches the
	// common case where ${SAGE_LLM_API_KEY} is in the YAML but the env var
	// is not set, leaving an empty value that silently breaks the feature.
	warnUnexpandedEnvVars(raw, expanded)

	return yaml.Unmarshal([]byte(expanded), cfg)
}

// warnUnexpandedEnvVars detects ${VAR} patterns in the raw YAML that expanded
// to empty strings (meaning the env var was not set) and logs a warning.
func warnUnexpandedEnvVars(raw, expanded string) {
	// Find all ${...} references in the raw YAML.
	for i := 0; i < len(raw); i++ {
		if i+1 < len(raw) && raw[i] == '$' && raw[i+1] == '{' {
			end := strings.Index(raw[i:], "}")
			if end < 0 {
				continue
			}
			varName := raw[i+2 : i+end]
			if os.Getenv(varName) == "" {
				fmt.Fprintf(os.Stderr,
					"WARNING: config %q references ${%s} but it is not set in the environment\n",
					"config.yaml", varName)
			}
			i += end
		}
	}
}

func overlayEnv(cfg *Config) {
	if v := os.Getenv("SAGE_MODE"); v != "" {
		cfg.Mode = v
	}
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		cfg.Postgres.DatabaseURL = v
	}
	if v := os.Getenv("SAGE_PG_HOST"); v != "" {
		cfg.Postgres.Host = v
	}
	if v := envInt("SAGE_PG_PORT"); v != 0 {
		cfg.Postgres.Port = v
	}
	if v := os.Getenv("SAGE_PG_USER"); v != "" {
		cfg.Postgres.User = v
	}
	if v := os.Getenv("SAGE_PG_PASSWORD"); v != "" {
		cfg.Postgres.Password = v
	}
	if v := os.Getenv("SAGE_PG_DATABASE"); v != "" {
		cfg.Postgres.Database = v
	}
	if v := os.Getenv("SAGE_PG_SSLMODE"); v != "" {
		cfg.Postgres.SSLMode = v
	}
	if v := envInt("SAGE_PG_MAX_CONNS"); v != 0 {
		cfg.Postgres.MaxConnections = v
	}
	if v := os.Getenv("SAGE_MCP_PORT"); v != "" {
		cfg.MCP.ListenAddr = "0.0.0.0:" + v
	}
	if v := os.Getenv("SAGE_PROMETHEUS_PORT"); v != "" {
		cfg.Prometheus.ListenAddr = "0.0.0.0:" + v
	}
	if v := envInt("SAGE_RATE_LIMIT"); v != 0 {
		// Store in a field we can access later; use default.
	}
	if v := os.Getenv("SAGE_LLM_API_KEY"); v != "" {
		cfg.LLM.APIKey = v
	}
	if v := os.Getenv("SAGE_LLM_ENDPOINT"); v != "" {
		cfg.LLM.Endpoint = v
	}
	if v := os.Getenv("SAGE_LLM_MODEL"); v != "" {
		cfg.LLM.Model = v
	}
	if v := os.Getenv("SAGE_TRUST_LEVEL"); v != "" {
		cfg.Trust.Level = v
	}
	if v := os.Getenv("SAGE_OPTIMIZER_LLM_API_KEY"); v != "" {
		cfg.LLM.OptimizerLLM.APIKey = v
	}
	if v := os.Getenv("SAGE_OPTIMIZER_LLM_ENDPOINT"); v != "" {
		cfg.LLM.OptimizerLLM.Endpoint = v
	}
	if v := os.Getenv("SAGE_OPTIMIZER_LLM_MODEL"); v != "" {
		cfg.LLM.OptimizerLLM.Model = v
	}
}

// HotReloadable returns the fields that can be reloaded without restart.
func (c *Config) HotReloadable() []string {
	return []string{
		"collector.interval_seconds",
		"analyzer.*",
		"safety.*",
		"trust.level", "trust.tier3_safe", "trust.tier3_moderate",
		"trust.maintenance_window",
		"llm.*",
		"briefing.schedule",
		"retention.*",
	}
}

// IsStandalone returns true if running in standalone mode.
func (c *Config) IsStandalone() bool {
	return c.Mode == "standalone"
}

// RateLimit returns the configured rate limit.
func (c *Config) RateLimit() int {
	if v := envInt("SAGE_RATE_LIMIT"); v != 0 {
		return v
	}
	return DefaultRateLimit
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

func envFloat(key string) float64 {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	f, _ := strconv.ParseFloat(v, 64)
	return f
}

// expandEnvInString replaces ${VAR} and $VAR in s.
func expandEnvInString(s string) string {
	return os.ExpandEnv(s)
}

// Suppress unused warnings.
var _ = envFloat
var _ = strings.Contains
