package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestYAMLRoundTrip_AllFieldsSurvive marshals a fully-populated Config to YAML
// and unmarshals it back, asserting every field survives the round trip. This
// catches struct tag typos, duplicate yaml keys, and fields that are silently
// dropped because they are nested under the wrong parent.
func TestYAMLRoundTrip_AllFieldsSurvive(t *testing.T) {
	original := Config{
		Mode: "standalone",
		Postgres: PostgresConfig{
			Host:           "pg-host",
			Port:           5433,
			User:           "pguser",
			Password:       "pgpass",
			Database:       "mydb",
			SSLMode:        "require",
			MaxConnections: 5,
			DatabaseURL:    "postgres://u:p@h:5432/db",
		},
		Collector: CollectorConfig{
			IntervalSeconds: 30,
			BatchSize:       500,
			MaxQueries:      250,
		},
		Analyzer: AnalyzerConfig{
			IntervalSeconds:              120,
			SlowQueryThresholdMs:         2000,
			SeqScanMinRows:               50000,
			UnusedIndexWindowDays:        14,
			IndexBloatThresholdPct:       25,
			TableBloatDeadTuplePct:       15,
			TableBloatMinRows:            5000,
			IdleInTxTimeoutMinutes:       60,
			CacheHitRatioWarning:         0.90,
			XIDWraparoundWarning:         400000000,
			XIDWraparoundCritical:        800000000,
			RegressionThresholdPct:       30,
			RegressionLookbackDays:       14,
			CheckpointFreqWarningPerHour: 6,
		},
		Safety: SafetyConfig{
			CPUCeilingPct:            80,
			QueryTimeoutMs:           1000,
			DDLTimeoutSeconds:        600,
			DiskPressureThresholdPct: 10,
			BackoffConsecutiveSkips:  5,
			DormantIntervalSeconds:   1200,
			LockTimeoutMs:            60000,
		},
		Trust: TrustConfig{
			Level:                 "advisory",
			RampStart:             "2025-01-01",
			MaintenanceWindow:     "02:00-06:00",
			Tier3Safe:             true,
			Tier3Moderate:         true,
			Tier3HighRisk:         true,
			RollbackThresholdPct:  20,
			RollbackWindowMinutes: 30,
			RollbackCooldownDays:  14,
			CascadeCooldownCycles: 5,
		},
		LLM: LLMConfig{
			Enabled:             true,
			Endpoint:            "https://llm.example.com",
			APIKey:              "sk-test-key",
			Model:               "gemini-pro",
			TimeoutSeconds:      60,
			TokenBudgetDaily:    1000000,
			ContextBudgetTokens: 16384,
			CooldownSeconds:     600,
			Optimizer: OptimizerConfig{
				Enabled:              true,
				MinQueryCalls:        50,
				MaxIndexesPerTable:   8,
				MaxNewPerTable:       2,
				MaxIncludeColumns:    4,
				OverIndexedRatioPct:  200,
				WriteHeavyRatioPct:   80,
				MinSnapshots:         3,
				HypoPGMinImprovePct:  15.0,
				PlanSource:           "hypopg",
				ConfidenceThreshold:  0.7,
				WriteImpactThreshPct: 20.0,
			},
			OptimizerLLM: OptimizerLLMConfig{
				Enabled:           true,
				Endpoint:          "https://opt-llm.example.com",
				APIKey:            "sk-opt-key",
				Model:             "gemini-2.0-flash-thinking",
				TimeoutSeconds:    180,
				TokenBudgetDaily:  750000,
				CooldownSeconds:   120,
				MaxOutputTokens:   16384,
				FallbackToGeneral: false,
			},
		},
		Advisor: AdvisorConfig{
			Enabled:           true,
			IntervalSeconds:   43200,
			VacuumEnabled:     true,
			WALEnabled:        false,
			ConnectionEnabled: true,
			MemoryEnabled:     false,
			RewriteEnabled:    true,
			BloatEnabled:      true,
		},
		Briefing: BriefingConfig{
			Schedule:        "0 8 * * 1-5",
			Channels:        []string{"slack", "email"},
			SlackWebhookURL: "https://hooks.slack.example.com/xyz",
		},
		Alerting: AlertingConfig{
			Enabled:              true,
			CheckIntervalSeconds: 30,
			CooldownMinutes:      10,
			QuietHoursStart:      "22:00",
			QuietHoursEnd:        "06:00",
			Timezone:             "America/New_York",
			SlackWebhookURL:      "https://hooks.slack.example.com/alerts",
			PagerDutyRoutingKey:  "pd-routing-key",
			Routes: []AlertRoute{
				{Severity: "critical", Channels: []string{"pagerduty", "slack"}},
				{Severity: "warning", Channels: []string{"slack"}},
			},
			Webhooks: []WebhookConfig{
				{
					Name:    "custom-hook",
					URL:     "https://webhook.example.com/alert",
					Headers: map[string]string{"Authorization": "Bearer tok"},
				},
			},
		},
		AutoExplain: AutoExplainConfig{
			Enabled:                false,
			LogMinDurationMs:       500,
			CollectIntervalSeconds: 120,
			MaxPlansPerCycle:       50,
			PreferSessionLoad:      false,
		},
		Forecaster: ForecasterConfig{
			Enabled:              true,
			LookbackDays:         60,
			DiskWarnGrowthGBDay:  10.0,
			ConnectionWarnPct:    70.0,
			CacheWarnThreshold:   0.90,
			SequenceWarnDays:     60,
			SequenceCriticalDays: 15,
		},
		Tuner: TunerConfig{
			Enabled:                true,
			LLMEnabled:             true,
			WorkMemMaxMB:           1024,
			PlanTimeRatio:          5.0,
			NestedLoopRowThreshold: 20000,
			ParallelMinTableRows:   500000,
			MinQueryCalls:          50,
			VerifyAfterApply:       true,
		},
		Retention: RetentionConfig{
			SnapshotsDays: 180,
			FindingsDays:  365,
			ActionsDays:   730,
			ExplainsDays:  180,
		},
		Prometheus: PrometheusConfig{
			ListenAddr: "0.0.0.0:9187",
		},
		MetaDB:        "postgres://meta@host/db",
		EncryptionKey: "secret-key-42",
	}

	data, err := yaml.Marshal(&original)
	if err != nil {
		t.Fatalf("yaml.Marshal failed: %v", err)
	}

	var roundtripped Config
	if err := yaml.Unmarshal(data, &roundtripped); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}

	// --- Top-level ---
	assertEqual(t, "Mode", roundtripped.Mode, original.Mode)
	assertEqual(t, "MetaDB", roundtripped.MetaDB, original.MetaDB)
	assertEqual(t, "EncryptionKey", roundtripped.EncryptionKey, original.EncryptionKey)

	// --- Postgres ---
	assertEqual(t, "Postgres.Host", roundtripped.Postgres.Host, original.Postgres.Host)
	assertEqualInt(t, "Postgres.Port", roundtripped.Postgres.Port, original.Postgres.Port)
	assertEqual(t, "Postgres.User", roundtripped.Postgres.User, original.Postgres.User)
	assertEqual(t, "Postgres.Password", roundtripped.Postgres.Password, original.Postgres.Password)
	assertEqual(t, "Postgres.Database", roundtripped.Postgres.Database, original.Postgres.Database)
	assertEqual(t, "Postgres.SSLMode", roundtripped.Postgres.SSLMode, original.Postgres.SSLMode)
	assertEqualInt(t, "Postgres.MaxConnections", roundtripped.Postgres.MaxConnections, original.Postgres.MaxConnections)
	assertEqual(t, "Postgres.DatabaseURL", roundtripped.Postgres.DatabaseURL, original.Postgres.DatabaseURL)

	// --- Collector ---
	assertEqualInt(t, "Collector.IntervalSeconds", roundtripped.Collector.IntervalSeconds, original.Collector.IntervalSeconds)
	assertEqualInt(t, "Collector.BatchSize", roundtripped.Collector.BatchSize, original.Collector.BatchSize)
	assertEqualInt(t, "Collector.MaxQueries", roundtripped.Collector.MaxQueries, original.Collector.MaxQueries)

	// --- Analyzer ---
	assertEqualInt(t, "Analyzer.IntervalSeconds", roundtripped.Analyzer.IntervalSeconds, original.Analyzer.IntervalSeconds)
	assertEqualInt(t, "Analyzer.SlowQueryThresholdMs", roundtripped.Analyzer.SlowQueryThresholdMs, original.Analyzer.SlowQueryThresholdMs)
	assertEqualInt(t, "Analyzer.SeqScanMinRows", roundtripped.Analyzer.SeqScanMinRows, original.Analyzer.SeqScanMinRows)
	assertEqualInt(t, "Analyzer.UnusedIndexWindowDays", roundtripped.Analyzer.UnusedIndexWindowDays, original.Analyzer.UnusedIndexWindowDays)
	assertEqualInt(t, "Analyzer.IndexBloatThresholdPct", roundtripped.Analyzer.IndexBloatThresholdPct, original.Analyzer.IndexBloatThresholdPct)
	assertEqualInt(t, "Analyzer.TableBloatDeadTuplePct", roundtripped.Analyzer.TableBloatDeadTuplePct, original.Analyzer.TableBloatDeadTuplePct)
	assertEqualInt(t, "Analyzer.TableBloatMinRows", roundtripped.Analyzer.TableBloatMinRows, original.Analyzer.TableBloatMinRows)
	assertEqualInt(t, "Analyzer.IdleInTxTimeoutMinutes", roundtripped.Analyzer.IdleInTxTimeoutMinutes, original.Analyzer.IdleInTxTimeoutMinutes)
	assertEqualFloat(t, "Analyzer.CacheHitRatioWarning", roundtripped.Analyzer.CacheHitRatioWarning, original.Analyzer.CacheHitRatioWarning)
	assertEqualInt64(t, "Analyzer.XIDWraparoundWarning", roundtripped.Analyzer.XIDWraparoundWarning, original.Analyzer.XIDWraparoundWarning)
	assertEqualInt64(t, "Analyzer.XIDWraparoundCritical", roundtripped.Analyzer.XIDWraparoundCritical, original.Analyzer.XIDWraparoundCritical)
	assertEqualInt(t, "Analyzer.RegressionThresholdPct", roundtripped.Analyzer.RegressionThresholdPct, original.Analyzer.RegressionThresholdPct)
	assertEqualInt(t, "Analyzer.RegressionLookbackDays", roundtripped.Analyzer.RegressionLookbackDays, original.Analyzer.RegressionLookbackDays)
	assertEqualInt(t, "Analyzer.CheckpointFreqWarningPerHour", roundtripped.Analyzer.CheckpointFreqWarningPerHour, original.Analyzer.CheckpointFreqWarningPerHour)

	// --- Safety ---
	assertEqualInt(t, "Safety.CPUCeilingPct", roundtripped.Safety.CPUCeilingPct, original.Safety.CPUCeilingPct)
	assertEqualInt(t, "Safety.QueryTimeoutMs", roundtripped.Safety.QueryTimeoutMs, original.Safety.QueryTimeoutMs)
	assertEqualInt(t, "Safety.DDLTimeoutSeconds", roundtripped.Safety.DDLTimeoutSeconds, original.Safety.DDLTimeoutSeconds)
	assertEqualInt(t, "Safety.DiskPressureThresholdPct", roundtripped.Safety.DiskPressureThresholdPct, original.Safety.DiskPressureThresholdPct)
	assertEqualInt(t, "Safety.BackoffConsecutiveSkips", roundtripped.Safety.BackoffConsecutiveSkips, original.Safety.BackoffConsecutiveSkips)
	assertEqualInt(t, "Safety.DormantIntervalSeconds", roundtripped.Safety.DormantIntervalSeconds, original.Safety.DormantIntervalSeconds)
	assertEqualInt(t, "Safety.LockTimeoutMs", roundtripped.Safety.LockTimeoutMs, original.Safety.LockTimeoutMs)

	// --- Trust ---
	assertEqual(t, "Trust.Level", roundtripped.Trust.Level, original.Trust.Level)
	assertEqual(t, "Trust.RampStart", roundtripped.Trust.RampStart, original.Trust.RampStart)
	assertEqual(t, "Trust.MaintenanceWindow", roundtripped.Trust.MaintenanceWindow, original.Trust.MaintenanceWindow)
	assertEqualBool(t, "Trust.Tier3Safe", roundtripped.Trust.Tier3Safe, original.Trust.Tier3Safe)
	assertEqualBool(t, "Trust.Tier3Moderate", roundtripped.Trust.Tier3Moderate, original.Trust.Tier3Moderate)
	assertEqualBool(t, "Trust.Tier3HighRisk", roundtripped.Trust.Tier3HighRisk, original.Trust.Tier3HighRisk)
	assertEqualInt(t, "Trust.RollbackThresholdPct", roundtripped.Trust.RollbackThresholdPct, original.Trust.RollbackThresholdPct)
	assertEqualInt(t, "Trust.RollbackWindowMinutes", roundtripped.Trust.RollbackWindowMinutes, original.Trust.RollbackWindowMinutes)
	assertEqualInt(t, "Trust.RollbackCooldownDays", roundtripped.Trust.RollbackCooldownDays, original.Trust.RollbackCooldownDays)
	assertEqualInt(t, "Trust.CascadeCooldownCycles", roundtripped.Trust.CascadeCooldownCycles, original.Trust.CascadeCooldownCycles)

	// --- LLM ---
	assertEqualBool(t, "LLM.Enabled", roundtripped.LLM.Enabled, original.LLM.Enabled)
	assertEqual(t, "LLM.Endpoint", roundtripped.LLM.Endpoint, original.LLM.Endpoint)
	assertEqual(t, "LLM.APIKey", roundtripped.LLM.APIKey, original.LLM.APIKey)
	assertEqual(t, "LLM.Model", roundtripped.LLM.Model, original.LLM.Model)
	assertEqualInt(t, "LLM.TimeoutSeconds", roundtripped.LLM.TimeoutSeconds, original.LLM.TimeoutSeconds)
	assertEqualInt(t, "LLM.TokenBudgetDaily", roundtripped.LLM.TokenBudgetDaily, original.LLM.TokenBudgetDaily)
	assertEqualInt(t, "LLM.ContextBudgetTokens", roundtripped.LLM.ContextBudgetTokens, original.LLM.ContextBudgetTokens)
	assertEqualInt(t, "LLM.CooldownSeconds", roundtripped.LLM.CooldownSeconds, original.LLM.CooldownSeconds)

	// --- LLM.Optimizer ---
	assertEqualBool(t, "LLM.Optimizer.Enabled", roundtripped.LLM.Optimizer.Enabled, original.LLM.Optimizer.Enabled)
	assertEqualInt(t, "LLM.Optimizer.MinQueryCalls", roundtripped.LLM.Optimizer.MinQueryCalls, original.LLM.Optimizer.MinQueryCalls)
	assertEqualInt(t, "LLM.Optimizer.MaxIndexesPerTable", roundtripped.LLM.Optimizer.MaxIndexesPerTable, original.LLM.Optimizer.MaxIndexesPerTable)
	assertEqualInt(t, "LLM.Optimizer.MaxNewPerTable", roundtripped.LLM.Optimizer.MaxNewPerTable, original.LLM.Optimizer.MaxNewPerTable)
	assertEqualInt(t, "LLM.Optimizer.MaxIncludeColumns", roundtripped.LLM.Optimizer.MaxIncludeColumns, original.LLM.Optimizer.MaxIncludeColumns)
	assertEqualInt(t, "LLM.Optimizer.OverIndexedRatioPct", roundtripped.LLM.Optimizer.OverIndexedRatioPct, original.LLM.Optimizer.OverIndexedRatioPct)
	assertEqualInt(t, "LLM.Optimizer.WriteHeavyRatioPct", roundtripped.LLM.Optimizer.WriteHeavyRatioPct, original.LLM.Optimizer.WriteHeavyRatioPct)
	assertEqualInt(t, "LLM.Optimizer.MinSnapshots", roundtripped.LLM.Optimizer.MinSnapshots, original.LLM.Optimizer.MinSnapshots)
	assertEqualFloat(t, "LLM.Optimizer.HypoPGMinImprovePct", roundtripped.LLM.Optimizer.HypoPGMinImprovePct, original.LLM.Optimizer.HypoPGMinImprovePct)
	assertEqual(t, "LLM.Optimizer.PlanSource", roundtripped.LLM.Optimizer.PlanSource, original.LLM.Optimizer.PlanSource)
	assertEqualFloat(t, "LLM.Optimizer.ConfidenceThreshold", roundtripped.LLM.Optimizer.ConfidenceThreshold, original.LLM.Optimizer.ConfidenceThreshold)
	assertEqualFloat(t, "LLM.Optimizer.WriteImpactThreshPct", roundtripped.LLM.Optimizer.WriteImpactThreshPct, original.LLM.Optimizer.WriteImpactThreshPct)

	// --- LLM.OptimizerLLM ---
	assertEqualBool(t, "LLM.OptimizerLLM.Enabled", roundtripped.LLM.OptimizerLLM.Enabled, original.LLM.OptimizerLLM.Enabled)
	assertEqual(t, "LLM.OptimizerLLM.Endpoint", roundtripped.LLM.OptimizerLLM.Endpoint, original.LLM.OptimizerLLM.Endpoint)
	assertEqual(t, "LLM.OptimizerLLM.APIKey", roundtripped.LLM.OptimizerLLM.APIKey, original.LLM.OptimizerLLM.APIKey)
	assertEqual(t, "LLM.OptimizerLLM.Model", roundtripped.LLM.OptimizerLLM.Model, original.LLM.OptimizerLLM.Model)
	assertEqualInt(t, "LLM.OptimizerLLM.TimeoutSeconds", roundtripped.LLM.OptimizerLLM.TimeoutSeconds, original.LLM.OptimizerLLM.TimeoutSeconds)
	assertEqualInt(t, "LLM.OptimizerLLM.TokenBudgetDaily", roundtripped.LLM.OptimizerLLM.TokenBudgetDaily, original.LLM.OptimizerLLM.TokenBudgetDaily)
	assertEqualInt(t, "LLM.OptimizerLLM.CooldownSeconds", roundtripped.LLM.OptimizerLLM.CooldownSeconds, original.LLM.OptimizerLLM.CooldownSeconds)
	assertEqualInt(t, "LLM.OptimizerLLM.MaxOutputTokens", roundtripped.LLM.OptimizerLLM.MaxOutputTokens, original.LLM.OptimizerLLM.MaxOutputTokens)
	assertEqualBool(t, "LLM.OptimizerLLM.FallbackToGeneral", roundtripped.LLM.OptimizerLLM.FallbackToGeneral, original.LLM.OptimizerLLM.FallbackToGeneral)

	// --- Advisor (top-level, NOT nested under LLM) ---
	assertEqualBool(t, "Advisor.Enabled", roundtripped.Advisor.Enabled, original.Advisor.Enabled)
	assertEqualInt(t, "Advisor.IntervalSeconds", roundtripped.Advisor.IntervalSeconds, original.Advisor.IntervalSeconds)
	assertEqualBool(t, "Advisor.VacuumEnabled", roundtripped.Advisor.VacuumEnabled, original.Advisor.VacuumEnabled)
	assertEqualBool(t, "Advisor.WALEnabled", roundtripped.Advisor.WALEnabled, original.Advisor.WALEnabled)
	assertEqualBool(t, "Advisor.ConnectionEnabled", roundtripped.Advisor.ConnectionEnabled, original.Advisor.ConnectionEnabled)
	assertEqualBool(t, "Advisor.MemoryEnabled", roundtripped.Advisor.MemoryEnabled, original.Advisor.MemoryEnabled)
	assertEqualBool(t, "Advisor.RewriteEnabled", roundtripped.Advisor.RewriteEnabled, original.Advisor.RewriteEnabled)
	assertEqualBool(t, "Advisor.BloatEnabled", roundtripped.Advisor.BloatEnabled, original.Advisor.BloatEnabled)

	// --- Briefing ---
	assertEqual(t, "Briefing.Schedule", roundtripped.Briefing.Schedule, original.Briefing.Schedule)
	assertEqual(t, "Briefing.SlackWebhookURL", roundtripped.Briefing.SlackWebhookURL, original.Briefing.SlackWebhookURL)
	if len(roundtripped.Briefing.Channels) != len(original.Briefing.Channels) {
		t.Errorf("Briefing.Channels length = %d, want %d",
			len(roundtripped.Briefing.Channels), len(original.Briefing.Channels))
	}

	// --- Alerting ---
	assertEqualBool(t, "Alerting.Enabled", roundtripped.Alerting.Enabled, original.Alerting.Enabled)
	assertEqualInt(t, "Alerting.CheckIntervalSeconds", roundtripped.Alerting.CheckIntervalSeconds, original.Alerting.CheckIntervalSeconds)
	assertEqualInt(t, "Alerting.CooldownMinutes", roundtripped.Alerting.CooldownMinutes, original.Alerting.CooldownMinutes)
	assertEqual(t, "Alerting.QuietHoursStart", roundtripped.Alerting.QuietHoursStart, original.Alerting.QuietHoursStart)
	assertEqual(t, "Alerting.QuietHoursEnd", roundtripped.Alerting.QuietHoursEnd, original.Alerting.QuietHoursEnd)
	assertEqual(t, "Alerting.Timezone", roundtripped.Alerting.Timezone, original.Alerting.Timezone)
	assertEqual(t, "Alerting.SlackWebhookURL", roundtripped.Alerting.SlackWebhookURL, original.Alerting.SlackWebhookURL)
	assertEqual(t, "Alerting.PagerDutyRoutingKey", roundtripped.Alerting.PagerDutyRoutingKey, original.Alerting.PagerDutyRoutingKey)
	if len(roundtripped.Alerting.Routes) != 2 {
		t.Errorf("Alerting.Routes length = %d, want 2", len(roundtripped.Alerting.Routes))
	}
	if len(roundtripped.Alerting.Webhooks) != 1 {
		t.Errorf("Alerting.Webhooks length = %d, want 1", len(roundtripped.Alerting.Webhooks))
	}

	// --- AutoExplain ---
	assertEqualBool(t, "AutoExplain.Enabled", roundtripped.AutoExplain.Enabled, original.AutoExplain.Enabled)
	assertEqualInt(t, "AutoExplain.LogMinDurationMs", roundtripped.AutoExplain.LogMinDurationMs, original.AutoExplain.LogMinDurationMs)
	assertEqualInt(t, "AutoExplain.CollectIntervalSeconds", roundtripped.AutoExplain.CollectIntervalSeconds, original.AutoExplain.CollectIntervalSeconds)
	assertEqualInt(t, "AutoExplain.MaxPlansPerCycle", roundtripped.AutoExplain.MaxPlansPerCycle, original.AutoExplain.MaxPlansPerCycle)
	assertEqualBool(t, "AutoExplain.PreferSessionLoad", roundtripped.AutoExplain.PreferSessionLoad, original.AutoExplain.PreferSessionLoad)

	// --- Forecaster ---
	assertEqualBool(t, "Forecaster.Enabled", roundtripped.Forecaster.Enabled, original.Forecaster.Enabled)
	assertEqualInt(t, "Forecaster.LookbackDays", roundtripped.Forecaster.LookbackDays, original.Forecaster.LookbackDays)
	assertEqualFloat(t, "Forecaster.DiskWarnGrowthGBDay", roundtripped.Forecaster.DiskWarnGrowthGBDay, original.Forecaster.DiskWarnGrowthGBDay)
	assertEqualFloat(t, "Forecaster.ConnectionWarnPct", roundtripped.Forecaster.ConnectionWarnPct, original.Forecaster.ConnectionWarnPct)
	assertEqualFloat(t, "Forecaster.CacheWarnThreshold", roundtripped.Forecaster.CacheWarnThreshold, original.Forecaster.CacheWarnThreshold)
	assertEqualInt(t, "Forecaster.SequenceWarnDays", roundtripped.Forecaster.SequenceWarnDays, original.Forecaster.SequenceWarnDays)
	assertEqualInt(t, "Forecaster.SequenceCriticalDays", roundtripped.Forecaster.SequenceCriticalDays, original.Forecaster.SequenceCriticalDays)

	// --- Tuner ---
	assertEqualBool(t, "Tuner.Enabled", roundtripped.Tuner.Enabled, original.Tuner.Enabled)
	assertEqualBool(t, "Tuner.LLMEnabled", roundtripped.Tuner.LLMEnabled, original.Tuner.LLMEnabled)
	assertEqualInt(t, "Tuner.WorkMemMaxMB", roundtripped.Tuner.WorkMemMaxMB, original.Tuner.WorkMemMaxMB)
	assertEqualFloat(t, "Tuner.PlanTimeRatio", roundtripped.Tuner.PlanTimeRatio, original.Tuner.PlanTimeRatio)
	assertEqualInt64(t, "Tuner.NestedLoopRowThreshold", roundtripped.Tuner.NestedLoopRowThreshold, original.Tuner.NestedLoopRowThreshold)
	assertEqualInt64(t, "Tuner.ParallelMinTableRows", roundtripped.Tuner.ParallelMinTableRows, original.Tuner.ParallelMinTableRows)
	assertEqualInt(t, "Tuner.MinQueryCalls", roundtripped.Tuner.MinQueryCalls, original.Tuner.MinQueryCalls)
	assertEqualBool(t, "Tuner.VerifyAfterApply", roundtripped.Tuner.VerifyAfterApply, original.Tuner.VerifyAfterApply)

	// --- Retention ---
	assertEqualInt(t, "Retention.SnapshotsDays", roundtripped.Retention.SnapshotsDays, original.Retention.SnapshotsDays)
	assertEqualInt(t, "Retention.FindingsDays", roundtripped.Retention.FindingsDays, original.Retention.FindingsDays)
	assertEqualInt(t, "Retention.ActionsDays", roundtripped.Retention.ActionsDays, original.Retention.ActionsDays)
	assertEqualInt(t, "Retention.ExplainsDays", roundtripped.Retention.ExplainsDays, original.Retention.ExplainsDays)

	// --- Prometheus ---
	assertEqual(t, "Prometheus.ListenAddr", roundtripped.Prometheus.ListenAddr, original.Prometheus.ListenAddr)
}

// TestYAMLParse_ExampleConfig parses realistic production-like YAML and
// verifies every section is populated. This is the test that would have
// caught the original bug: advisor nested under llm is silently ignored.
func TestYAMLParse_ExampleConfig(t *testing.T) {
	yamlStr := `
mode: standalone

postgres:
  host: db.prod.internal
  port: 5432
  user: sage_agent
  password: hunter2
  database: app_production
  sslmode: require
  max_connections: 3

collector:
  interval_seconds: 30
  batch_size: 2000
  max_queries: 1000

analyzer:
  interval_seconds: 300
  slow_query_threshold_ms: 500
  seq_scan_min_rows: 50000
  unused_index_window_days: 14
  index_bloat_threshold_pct: 25
  table_bloat_dead_tuple_pct: 15
  table_bloat_min_rows: 5000
  idle_in_transaction_timeout_minutes: 15
  cache_hit_ratio_warning: 0.97
  xid_wraparound_warning: 400000000
  xid_wraparound_critical: 900000000
  regression_threshold_pct: 25
  regression_lookback_days: 14
  checkpoint_frequency_warning_per_hour: 8

safety:
  cpu_ceiling_pct: 85
  query_timeout_ms: 750
  ddl_timeout_seconds: 600
  disk_pressure_threshold_pct: 8
  backoff_consecutive_skips: 4
  dormant_interval_seconds: 900
  lock_timeout_ms: 45000

trust:
  level: advisory
  ramp_start: "2025-06-01"
  maintenance_window: "02:00-05:00"
  tier3_safe: true
  tier3_moderate: true
  tier3_high_risk: false
  rollback_threshold_pct: 15
  rollback_window_minutes: 20
  rollback_cooldown_days: 10
  cascade_cooldown_cycles: 4

llm:
  enabled: true
  endpoint: https://generativelanguage.googleapis.com
  api_key: AIza-fake-key
  model: gemini-2.0-flash
  timeout_seconds: 45
  token_budget_daily: 750000
  context_budget_tokens: 16384
  cooldown_seconds: 120
  optimizer:
    enabled: true
    min_query_calls: 50
    max_indexes_per_table: 8
    max_new_per_table: 2
    max_include_columns: 4
    over_indexed_ratio_pct: 175
    write_heavy_ratio_pct: 65
    min_snapshots: 3
    hypopg_min_improvement_pct: 12.5
    plan_source: hypopg
    confidence_threshold: 0.6
    write_impact_threshold_pct: 18.0
  optimizer_llm:
    enabled: true
    endpoint: https://generativelanguage.googleapis.com
    api_key: AIza-opt-key
    model: gemini-2.5-pro
    timeout_seconds: 180
    token_budget_daily: 500000
    cooldown_seconds: 600
    max_output_tokens: 16384
    fallback_to_general: true

advisor:
  enabled: true
  interval_seconds: 43200
  vacuum_enabled: true
  wal_enabled: true
  connection_enabled: true
  memory_enabled: true
  rewrite_enabled: false
  bloat_enabled: true

alerting:
  enabled: true
  check_interval_seconds: 45
  cooldown_minutes: 10
  quiet_hours_start: "23:00"
  quiet_hours_end: "07:00"
  timezone: America/Chicago
  slack_webhook_url: https://hooks.slack.com/services/T00/B00/xxx
  routes:
    - severity: critical
      channels: [pagerduty, slack]
    - severity: warning
      channels: [slack]

retention:
  snapshots_days: 120
  findings_days: 240
  actions_days: 400
  explains_days: 120

prometheus:
  listen_addr: 0.0.0.0:9187
`

	var cfg Config
	if err := yaml.Unmarshal([]byte(yamlStr), &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}

	// Collector populated
	assertEqualInt(t, "Collector.IntervalSeconds", cfg.Collector.IntervalSeconds, 30)
	assertEqualInt(t, "Collector.BatchSize", cfg.Collector.BatchSize, 2000)
	assertEqualInt(t, "Collector.MaxQueries", cfg.Collector.MaxQueries, 1000)

	// Analyzer populated
	assertEqualInt(t, "Analyzer.IntervalSeconds", cfg.Analyzer.IntervalSeconds, 300)
	assertEqualInt(t, "Analyzer.SlowQueryThresholdMs", cfg.Analyzer.SlowQueryThresholdMs, 500)
	assertEqualInt(t, "Analyzer.UnusedIndexWindowDays", cfg.Analyzer.UnusedIndexWindowDays, 14)

	// Trust populated
	assertEqual(t, "Trust.Level", cfg.Trust.Level, "advisory")
	assertEqualBool(t, "Trust.Tier3Moderate", cfg.Trust.Tier3Moderate, true)

	// Safety populated
	assertEqualInt(t, "Safety.CPUCeilingPct", cfg.Safety.CPUCeilingPct, 85)
	assertEqualInt(t, "Safety.LockTimeoutMs", cfg.Safety.LockTimeoutMs, 45000)

	// LLM populated
	assertEqualBool(t, "LLM.Enabled", cfg.LLM.Enabled, true)
	assertEqual(t, "LLM.Model", cfg.LLM.Model, "gemini-2.0-flash")
	assertEqualInt(t, "LLM.ContextBudgetTokens", cfg.LLM.ContextBudgetTokens, 16384)

	// LLM.Optimizer populated (nested under llm)
	assertEqualBool(t, "LLM.Optimizer.Enabled", cfg.LLM.Optimizer.Enabled, true)
	assertEqualInt(t, "LLM.Optimizer.MinQueryCalls", cfg.LLM.Optimizer.MinQueryCalls, 50)
	assertEqualFloat(t, "LLM.Optimizer.ConfidenceThreshold", cfg.LLM.Optimizer.ConfidenceThreshold, 0.6)

	// LLM.OptimizerLLM populated (nested under llm)
	assertEqualBool(t, "LLM.OptimizerLLM.Enabled", cfg.LLM.OptimizerLLM.Enabled, true)
	assertEqual(t, "LLM.OptimizerLLM.Model", cfg.LLM.OptimizerLLM.Model, "gemini-2.5-pro")

	// Advisor populated at TOP LEVEL (the critical assertion)
	assertEqualBool(t, "Advisor.Enabled", cfg.Advisor.Enabled, true)
	assertEqualInt(t, "Advisor.IntervalSeconds", cfg.Advisor.IntervalSeconds, 43200)
	assertEqualBool(t, "Advisor.VacuumEnabled", cfg.Advisor.VacuumEnabled, true)
	assertEqualBool(t, "Advisor.WALEnabled", cfg.Advisor.WALEnabled, true)
	assertEqualBool(t, "Advisor.ConnectionEnabled", cfg.Advisor.ConnectionEnabled, true)
	assertEqualBool(t, "Advisor.MemoryEnabled", cfg.Advisor.MemoryEnabled, true)
	assertEqualBool(t, "Advisor.RewriteEnabled", cfg.Advisor.RewriteEnabled, false)
	assertEqualBool(t, "Advisor.BloatEnabled", cfg.Advisor.BloatEnabled, true)

	// Alerting populated
	assertEqualBool(t, "Alerting.Enabled", cfg.Alerting.Enabled, true)
	assertEqualInt(t, "Alerting.CheckIntervalSeconds", cfg.Alerting.CheckIntervalSeconds, 45)
	assertEqual(t, "Alerting.Timezone", cfg.Alerting.Timezone, "America/Chicago")
	if len(cfg.Alerting.Routes) != 2 {
		t.Errorf("Alerting.Routes length = %d, want 2", len(cfg.Alerting.Routes))
	}

	// Retention populated
	assertEqualInt(t, "Retention.SnapshotsDays", cfg.Retention.SnapshotsDays, 120)
	assertEqualInt(t, "Retention.FindingsDays", cfg.Retention.FindingsDays, 240)
	assertEqualInt(t, "Retention.ActionsDays", cfg.Retention.ActionsDays, 400)
	assertEqualInt(t, "Retention.ExplainsDays", cfg.Retention.ExplainsDays, 120)
}

// TestYAMLParse_AdvisorUnderLLM_IsIgnored is a regression test for the exact
// bug found in production: placing `advisor:` under `llm:` causes yaml.v3 to
// silently drop it because LLMConfig has no Advisor field.
func TestYAMLParse_AdvisorUnderLLM_IsIgnored(t *testing.T) {
	yamlStr := `
llm:
  enabled: true
  model: gemini-2.0-flash
  advisor:
    enabled: true
    interval_seconds: 3600
    vacuum_enabled: true
`

	var cfg Config
	if err := yaml.Unmarshal([]byte(yamlStr), &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}

	// The advisor block was nested under llm, so it should be silently
	// ignored by yaml.v3. The top-level Advisor fields should remain at
	// their zero values.
	if cfg.Advisor.Enabled {
		t.Errorf("Advisor.Enabled = true, want false " +
			"(advisor nested under llm should be silently ignored)")
	}
	if cfg.Advisor.IntervalSeconds != 0 {
		t.Errorf("Advisor.IntervalSeconds = %d, want 0 "+
			"(advisor nested under llm should be silently ignored)",
			cfg.Advisor.IntervalSeconds)
	}
	if cfg.Advisor.VacuumEnabled {
		t.Errorf("Advisor.VacuumEnabled = true, want false " +
			"(advisor nested under llm should be silently ignored)")
	}

	// LLM fields that ARE valid should still parse.
	assertEqualBool(t, "LLM.Enabled", cfg.LLM.Enabled, true)
	assertEqual(t, "LLM.Model", cfg.LLM.Model, "gemini-2.0-flash")
}

// TestYAMLParse_OptimizerUnderLLM_Works verifies that `optimizer:` nested
// under `llm:` correctly populates LLMConfig.Optimizer, since that field
// actually exists on LLMConfig (unlike advisor).
func TestYAMLParse_OptimizerUnderLLM_Works(t *testing.T) {
	yamlStr := `
llm:
  enabled: true
  optimizer:
    enabled: true
    min_query_calls: 75
    max_indexes_per_table: 12
    confidence_threshold: 0.8
`

	var cfg Config
	if err := yaml.Unmarshal([]byte(yamlStr), &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}

	assertEqualBool(t, "LLM.Enabled", cfg.LLM.Enabled, true)
	assertEqualBool(t, "LLM.Optimizer.Enabled", cfg.LLM.Optimizer.Enabled, true)
	assertEqualInt(t, "LLM.Optimizer.MinQueryCalls", cfg.LLM.Optimizer.MinQueryCalls, 75)
	assertEqualInt(t, "LLM.Optimizer.MaxIndexesPerTable", cfg.LLM.Optimizer.MaxIndexesPerTable, 12)
	assertEqualFloat(t, "LLM.Optimizer.ConfidenceThreshold", cfg.LLM.Optimizer.ConfidenceThreshold, 0.8)
}

// TestDefaultConfig_NonZeroFields verifies that newDefaults() sets critical
// fields to their intended non-zero values. This catches the class of bugs
// where a default constant is wrong (e.g., context budget was 4096 instead
// of 8192).
func TestDefaultConfig_NonZeroFields(t *testing.T) {
	cfg := newDefaults()

	// Mode
	assertEqual(t, "Mode", cfg.Mode, "standalone")

	// Collector: interval should be 60s, not 0
	assertEqualInt(t, "Collector.IntervalSeconds",
		cfg.Collector.IntervalSeconds,
		int(DefaultCollectorInterval/time.Second))
	if cfg.Collector.IntervalSeconds == 0 {
		t.Error("Collector.IntervalSeconds is 0 (should never be zero)")
	}

	// Collector batch size and max queries
	assertEqualInt(t, "Collector.BatchSize",
		cfg.Collector.BatchSize, DefaultCollectorBatchSize)
	assertEqualInt(t, "Collector.MaxQueries",
		cfg.Collector.MaxQueries, DefaultCollectorMaxQueries)

	// Analyzer: slow query threshold
	assertEqualInt(t, "Analyzer.SlowQueryThresholdMs",
		cfg.Analyzer.SlowQueryThresholdMs, DefaultSlowQueryThresholdMs)
	if cfg.Analyzer.SlowQueryThresholdMs == 0 {
		t.Error("Analyzer.SlowQueryThresholdMs is 0 (should never be zero)")
	}

	// Analyzer: unused index window -- the "default was 0 but should be 7" bug
	assertEqualInt(t, "Analyzer.UnusedIndexWindowDays",
		cfg.Analyzer.UnusedIndexWindowDays, DefaultUnusedIndexWindowDays)
	if cfg.Analyzer.UnusedIndexWindowDays == 0 {
		t.Error("Analyzer.UnusedIndexWindowDays is 0 " +
			"(must be 7, not zero -- this was a known bug)")
	}

	// LLM context budget: must be 8192, not 4096
	assertEqualInt(t, "LLM.ContextBudgetTokens",
		cfg.LLM.ContextBudgetTokens, DefaultLLMContextBudget)
	if cfg.LLM.ContextBudgetTokens == 4096 {
		t.Error("LLM.ContextBudgetTokens is 4096 " +
			"(must be 8192 -- this was a known bug)")
	}
	if cfg.LLM.ContextBudgetTokens != 8192 {
		t.Errorf("LLM.ContextBudgetTokens = %d, want exactly 8192",
			cfg.LLM.ContextBudgetTokens)
	}

	// Safety: CPU ceiling
	assertEqualInt(t, "Safety.CPUCeilingPct",
		cfg.Safety.CPUCeilingPct, DefaultCPUCeilingPct)
	if cfg.Safety.CPUCeilingPct == 0 {
		t.Error("Safety.CPUCeilingPct is 0 (should be 90)")
	}

	// Safety: query timeout
	assertEqualInt(t, "Safety.QueryTimeoutMs",
		cfg.Safety.QueryTimeoutMs, DefaultQueryTimeoutMs)

	// Safety: lock timeout
	assertEqualInt(t, "Safety.LockTimeoutMs",
		cfg.Safety.LockTimeoutMs, DefaultLockTimeoutMs)

	// Trust: level should be "observation"
	assertEqual(t, "Trust.Level", cfg.Trust.Level, DefaultTrustLevel)

	// Trust: tier3 defaults
	assertEqualBool(t, "Trust.Tier3Safe", cfg.Trust.Tier3Safe, DefaultTier3Safe)
	assertEqualBool(t, "Trust.Tier3Moderate", cfg.Trust.Tier3Moderate, DefaultTier3Moderate)
	assertEqualBool(t, "Trust.Tier3HighRisk", cfg.Trust.Tier3HighRisk, DefaultTier3HighRisk)

	// Retention: non-zero
	assertEqualInt(t, "Retention.SnapshotsDays",
		cfg.Retention.SnapshotsDays, DefaultRetentionSnapshotsDays)
	if cfg.Retention.SnapshotsDays == 0 {
		t.Error("Retention.SnapshotsDays is 0 (should be 90)")
	}

	// Optimizer v2 defaults
	assertEqualFloat(t, "LLM.Optimizer.ConfidenceThreshold",
		cfg.LLM.Optimizer.ConfidenceThreshold, DefaultOptConfidenceThreshold)
	if cfg.LLM.Optimizer.ConfidenceThreshold == 0 {
		t.Error("LLM.Optimizer.ConfidenceThreshold is 0 " +
			"(should be 0.5)")
	}
	assertEqual(t, "LLM.Optimizer.PlanSource",
		cfg.LLM.Optimizer.PlanSource, DefaultOptPlanSource)

	// Advisor defaults: enabled=false but sub-features default to true
	assertEqualBool(t, "Advisor.Enabled", cfg.Advisor.Enabled, false)
	assertEqualBool(t, "Advisor.VacuumEnabled", cfg.Advisor.VacuumEnabled, true)
	assertEqualBool(t, "Advisor.WALEnabled", cfg.Advisor.WALEnabled, true)

	// Briefing default schedule
	assertEqual(t, "Briefing.Schedule", cfg.Briefing.Schedule, DefaultBriefingSchedule)

	// Prometheus listen addr
	assertEqual(t, "Prometheus.ListenAddr",
		cfg.Prometheus.ListenAddr, DefaultPrometheusListenAddr)

	// Postgres defaults
	assertEqualInt(t, "Postgres.Port", cfg.Postgres.Port, DefaultPGPort)
	assertEqual(t, "Postgres.User", cfg.Postgres.User, DefaultPGUser)
	assertEqual(t, "Postgres.SSLMode", cfg.Postgres.SSLMode, DefaultPGSSLMode)

	// Forecaster defaults
	assertEqualBool(t, "Forecaster.Enabled", cfg.Forecaster.Enabled, true)
	assertEqualInt(t, "Forecaster.LookbackDays",
		cfg.Forecaster.LookbackDays, DefaultForecasterLookbackDays)

	// Tuner defaults
	assertEqualBool(t, "Tuner.Enabled", cfg.Tuner.Enabled, true)
	assertEqualInt(t, "Tuner.WorkMemMaxMB",
		cfg.Tuner.WorkMemMaxMB, DefaultTunerWorkMemMaxMB)

	// AutoExplain defaults
	assertEqualBool(t, "AutoExplain.Enabled", cfg.AutoExplain.Enabled, true)
	assertEqualInt(t, "AutoExplain.LogMinDurationMs",
		cfg.AutoExplain.LogMinDurationMs, DefaultAutoExplainLogMinDuration)
}

// TestYAMLDecode_KnownFields_RejectsUnknownKeys uses yaml.Decoder with
// KnownFields(true) to verify that every key in a fully-populated YAML
// document maps to a real struct field. This catches silent drops where
// a key is misspelled or placed under the wrong parent.
func TestYAMLDecode_KnownFields_RejectsUnknownKeys(t *testing.T) {
	// Valid YAML that should decode without error.
	validYAML := `
mode: standalone
postgres:
  host: localhost
  port: 5432
  user: sage
  password: pass
  database: testdb
  sslmode: prefer
  max_connections: 2
  database_url: ""
collector:
  interval_seconds: 60
  batch_size: 1000
  max_queries: 500
analyzer:
  interval_seconds: 600
  slow_query_threshold_ms: 1000
  seq_scan_min_rows: 100000
  unused_index_window_days: 7
  index_bloat_threshold_pct: 30
  table_bloat_dead_tuple_pct: 20
  table_bloat_min_rows: 1000
  idle_in_transaction_timeout_minutes: 30
  cache_hit_ratio_warning: 0.95
  xid_wraparound_warning: 500000000
  xid_wraparound_critical: 1000000000
  regression_threshold_pct: 50
  regression_lookback_days: 7
  checkpoint_frequency_warning_per_hour: 12
  work_mem_promotion_threshold: 5
safety:
  cpu_ceiling_pct: 90
  query_timeout_ms: 500
  ddl_timeout_seconds: 300
  disk_pressure_threshold_pct: 5
  backoff_consecutive_skips: 3
  dormant_interval_seconds: 600
  lock_timeout_ms: 30000
trust:
  level: observation
  ramp_start: ""
  maintenance_window: ""
  tier3_safe: true
  tier3_moderate: false
  tier3_high_risk: false
  rollback_threshold_pct: 10
  rollback_window_minutes: 15
  rollback_cooldown_days: 7
  cascade_cooldown_cycles: 3
llm:
  enabled: false
  endpoint: ""
  api_key: ""
  model: ""
  timeout_seconds: 30
  token_budget_daily: 500000
  context_budget_tokens: 8192
  cooldown_seconds: 300
  optimizer:
    enabled: false
    min_query_calls: 100
    max_indexes_per_table: 10
    max_new_per_table: 3
    max_include_columns: 3
    over_indexed_ratio_pct: 150
    write_heavy_ratio_pct: 70
    min_snapshots: 3
    hypopg_min_improvement_pct: 15.0
    plan_source: hypopg
    confidence_threshold: 0.5
    write_impact_threshold_pct: 20.0
  optimizer_llm:
    enabled: false
    endpoint: ""
    api_key: ""
    model: ""
    timeout_seconds: 120
    token_budget_daily: 500000
    cooldown_seconds: 300
    max_output_tokens: 16384
    fallback_to_general: true
advisor:
  enabled: false
  interval_seconds: 3600
  vacuum_enabled: true
  wal_enabled: true
  connection_enabled: true
  memory_enabled: true
  rewrite_enabled: true
  bloat_enabled: true
briefing:
  schedule: "0 6 * * *"
  channels: ["stdout"]
  slack_webhook_url: ""
alerting:
  enabled: false
  check_interval_seconds: 60
  cooldown_minutes: 15
  quiet_hours_start: ""
  quiet_hours_end: ""
  timezone: UTC
  slack_webhook_url: ""
  pagerduty_routing_key: ""
auto_explain:
  enabled: true
  log_min_duration_ms: 1000
  collect_interval_seconds: 300
  max_plans_per_cycle: 100
  prefer_session_load: true
forecaster:
  enabled: true
  lookback_days: 30
  disk_warn_growth_gb_day: 5.0
  connection_warn_pct: 80.0
  cache_warn_threshold: 0.95
  sequence_warn_days: 90
  sequence_critical_days: 30
tuner:
  enabled: true
  llm_enabled: false
  work_mem_max_mb: 512
  plan_time_ratio: 3.0
  nested_loop_row_threshold: 10000
  parallel_min_table_rows: 1000000
  min_query_calls: 100
  verify_after_apply: true
  hint_retirement_days: 14
  revalidation_interval_hours: 24
  revalidation_keep_ratio: 1.2
  revalidation_rollback_ratio: 0.8
  revalidation_explain_timeout_ms: 10000
  stale_stats_estimate_skew: 10.0
  stale_stats_mod_ratio: 0.1
  stale_stats_age_minutes: 60
  analyze_max_table_mb: 10240
  analyze_cooldown_minutes: 60
  analyze_timeout_ms: 600000
  max_concurrent_analyze: 1
retention:
  snapshots_days: 90
  findings_days: 180
  actions_days: 365
  explains_days: 90
prometheus:
  listen_addr: "0.0.0.0:9187"
oauth:
  enabled: false
  provider: google
  client_id: ""
  client_secret: ""
  redirect_url: ""
  issuer_url: ""
  default_role: viewer
api:
  listen_addr: "0.0.0.0:8080"
meta_db: ""
encryption_key: ""
`

	dec := yaml.NewDecoder(strings.NewReader(validYAML))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("KnownFields decode of valid YAML failed: %v "+
			"(a struct field is missing its yaml tag or a key "+
			"is misspelled)", err)
	}

	// Also verify a document with a typo IS rejected.
	typoYAML := `
mode: standalone
postgress:
  host: localhost
`
	dec2 := yaml.NewDecoder(strings.NewReader(typoYAML))
	dec2.KnownFields(true)
	var cfg2 Config
	if err := dec2.Decode(&cfg2); err == nil {
		t.Error("KnownFields should reject unknown key " +
			"'postgress' (note double s)")
	}
}

// TestYAMLRoundTrip_OAuthFields verifies OAuth sub-struct round-trips.
func TestYAMLRoundTrip_OAuthFields(t *testing.T) {
	original := Config{
		OAuth: OAuthConfig{
			Enabled:      true,
			Provider:     "google",
			ClientID:     "client-123",
			ClientSecret: "secret-456",
			RedirectURL:  "https://example.com/callback",
			IssuerURL:    "https://accounts.google.com",
			DefaultRole:  "viewer",
		},
	}

	data, err := yaml.Marshal(&original)
	if err != nil {
		t.Fatalf("yaml.Marshal failed: %v", err)
	}

	var roundtripped Config
	if err := yaml.Unmarshal(data, &roundtripped); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}

	assertEqualBool(t, "OAuth.Enabled",
		roundtripped.OAuth.Enabled, true)
	assertEqual(t, "OAuth.Provider",
		roundtripped.OAuth.Provider, "google")
	assertEqual(t, "OAuth.ClientID",
		roundtripped.OAuth.ClientID, "client-123")
	assertEqual(t, "OAuth.ClientSecret",
		roundtripped.OAuth.ClientSecret, "secret-456")
	assertEqual(t, "OAuth.RedirectURL",
		roundtripped.OAuth.RedirectURL, "https://example.com/callback")
	assertEqual(t, "OAuth.IssuerURL",
		roundtripped.OAuth.IssuerURL, "https://accounts.google.com")
	assertEqual(t, "OAuth.DefaultRole",
		roundtripped.OAuth.DefaultRole, "viewer")
}

// TestYAMLRoundTrip_FleetFields verifies fleet mode sub-structs
// (Databases, Defaults, API) round-trip correctly.
func TestYAMLRoundTrip_FleetFields(t *testing.T) {
	boolTrue := true
	boolFalse := false
	original := Config{
		Mode: "fleet",
		Databases: []DatabaseConfig{
			{
				Name:                     "prod-primary",
				Host:                     "db1.prod",
				Port:                     5432,
				User:                     "sage",
				Password:                 "secret",
				Database:                 "app",
				SSLMode:                  "require",
				MaxConnections:           3,
				Tags:                     []string{"tier=prod", "team=payments"},
				TrustLevel:               "advisory",
				ExecutorEnabled:          &boolTrue,
				LLMEnabled:               &boolFalse,
				CollectorIntervalSeconds: 30,
				AnalyzerIntervalSeconds:  600,
			},
		},
		Defaults: DefaultsConfig{
			MaxConnections:           2,
			TrustLevel:               "observation",
			CollectorIntervalSeconds: 60,
			AnalyzerIntervalSeconds:  300,
		},
		API: APIConfig{
			ListenAddr: "0.0.0.0:8080",
		},
	}

	data, err := yaml.Marshal(&original)
	if err != nil {
		t.Fatalf("yaml.Marshal failed: %v", err)
	}

	var rt Config
	if err := yaml.Unmarshal(data, &rt); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}

	// Databases
	if len(rt.Databases) != 1 {
		t.Fatalf("Databases length = %d, want 1", len(rt.Databases))
	}
	db := rt.Databases[0]
	assertEqual(t, "Databases[0].Name", db.Name, "prod-primary")
	assertEqual(t, "Databases[0].Host", db.Host, "db1.prod")
	assertEqualInt(t, "Databases[0].Port", db.Port, 5432)
	assertEqual(t, "Databases[0].User", db.User, "sage")
	assertEqual(t, "Databases[0].Password", db.Password, "secret")
	assertEqual(t, "Databases[0].Database", db.Database, "app")
	assertEqual(t, "Databases[0].SSLMode", db.SSLMode, "require")
	assertEqualInt(t, "Databases[0].MaxConnections",
		db.MaxConnections, 3)
	if len(db.Tags) != 2 {
		t.Errorf("Databases[0].Tags length = %d, want 2",
			len(db.Tags))
	}
	assertEqual(t, "Databases[0].TrustLevel",
		db.TrustLevel, "advisory")
	if db.ExecutorEnabled == nil || !*db.ExecutorEnabled {
		t.Error("Databases[0].ExecutorEnabled should be true")
	}
	if db.LLMEnabled == nil || *db.LLMEnabled {
		t.Error("Databases[0].LLMEnabled should be false")
	}
	assertEqualInt(t, "Databases[0].CollectorIntervalSeconds",
		db.CollectorIntervalSeconds, 30)
	assertEqualInt(t, "Databases[0].AnalyzerIntervalSeconds",
		db.AnalyzerIntervalSeconds, 600)

	// Defaults
	assertEqualInt(t, "Defaults.MaxConnections",
		rt.Defaults.MaxConnections, 2)
	assertEqual(t, "Defaults.TrustLevel",
		rt.Defaults.TrustLevel, "observation")
	assertEqualInt(t, "Defaults.CollectorIntervalSeconds",
		rt.Defaults.CollectorIntervalSeconds, 60)
	assertEqualInt(t, "Defaults.AnalyzerIntervalSeconds",
		rt.Defaults.AnalyzerIntervalSeconds, 300)

	// API
	assertEqual(t, "API.ListenAddr",
		rt.API.ListenAddr, "0.0.0.0:8080")
}

// TestYAMLRoundTrip_TunerV085Fields verifies the v0.8.5 tuner fields
// (hint revalidation + stale stats) survive YAML round-trip.
func TestYAMLRoundTrip_TunerV085Fields(t *testing.T) {
	original := Config{
		Tuner: TunerConfig{
			Enabled:                      true,
			VerifyAfterApply:             true,
			HintRetirementDays:           14,
			RevalidationIntervalHours:    24,
			RevalidationKeepRatio:        1.2,
			RevalidationRollbackRatio:    0.8,
			RevalidationExplainTimeoutMs: 10000,
			StaleStatsEstimateSkew:       10.0,
			StaleStatsModRatio:           0.1,
			StaleStatsAgeMinutes:         60,
			AnalyzeMaxTableMB:            10240,
			AnalyzeCooldownMinutes:       60,
			AnalyzeTimeoutMs:             600000,
			MaxConcurrentAnalyze:         1,
		},
	}

	data, err := yaml.Marshal(&original)
	if err != nil {
		t.Fatalf("yaml.Marshal failed: %v", err)
	}

	var rt Config
	if err := yaml.Unmarshal(data, &rt); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}

	assertEqualInt(t, "Tuner.HintRetirementDays",
		rt.Tuner.HintRetirementDays, 14)
	assertEqualInt(t, "Tuner.RevalidationIntervalHours",
		rt.Tuner.RevalidationIntervalHours, 24)
	assertEqualFloat(t, "Tuner.RevalidationKeepRatio",
		rt.Tuner.RevalidationKeepRatio, 1.2)
	assertEqualFloat(t, "Tuner.RevalidationRollbackRatio",
		rt.Tuner.RevalidationRollbackRatio, 0.8)
	assertEqualInt(t, "Tuner.RevalidationExplainTimeoutMs",
		rt.Tuner.RevalidationExplainTimeoutMs, 10000)
	assertEqualFloat(t, "Tuner.StaleStatsEstimateSkew",
		rt.Tuner.StaleStatsEstimateSkew, 10.0)
	assertEqualFloat(t, "Tuner.StaleStatsModRatio",
		rt.Tuner.StaleStatsModRatio, 0.1)
	assertEqualInt(t, "Tuner.StaleStatsAgeMinutes",
		rt.Tuner.StaleStatsAgeMinutes, 60)
	assertEqualInt64(t, "Tuner.AnalyzeMaxTableMB",
		rt.Tuner.AnalyzeMaxTableMB, 10240)
	assertEqualInt(t, "Tuner.AnalyzeCooldownMinutes",
		rt.Tuner.AnalyzeCooldownMinutes, 60)
	assertEqualInt(t, "Tuner.AnalyzeTimeoutMs",
		rt.Tuner.AnalyzeTimeoutMs, 600000)
	assertEqualInt(t, "Tuner.MaxConcurrentAnalyze",
		rt.Tuner.MaxConcurrentAnalyze, 1)
}

// TestYAMLRoundTrip_AnalyzerV085Field verifies the work_mem_promotion_threshold
// field survives round-trip.
func TestYAMLRoundTrip_AnalyzerV085Field(t *testing.T) {
	original := Config{
		Analyzer: AnalyzerConfig{
			WorkMemPromotionThreshold: 5,
		},
	}

	data, err := yaml.Marshal(&original)
	if err != nil {
		t.Fatalf("yaml.Marshal failed: %v", err)
	}

	var rt Config
	if err := yaml.Unmarshal(data, &rt); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}

	assertEqualInt(t, "Analyzer.WorkMemPromotionThreshold",
		rt.Analyzer.WorkMemPromotionThreshold, 5)
}

// --- Test helpers (stdlib only, matching existing test style) ---

func assertEqual(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}

func assertEqualInt(t *testing.T, field string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", field, got, want)
	}
}

func assertEqualInt64(t *testing.T, field string, got, want int64) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", field, got, want)
	}
}

func assertEqualFloat(t *testing.T, field string, got, want float64) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %f, want %f", field, got, want)
	}
}

func assertEqualBool(t *testing.T, field string, got, want bool) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", field, got, want)
	}
}
