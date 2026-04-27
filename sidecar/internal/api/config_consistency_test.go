package api

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// hotReloadTestValue defines a test input string and a function
// that reads the corresponding field from the config struct,
// returning its current value as a string for comparison.
type hotReloadTestValue struct {
	input  string                      // value passed to hotReload
	reader func(*config.Config) string // reads the mutated field
}

// hotReloadTestValues maps every key in allowedConfigKeys to a
// test value and a reader that extracts the corresponding config
// field. The input must differ from the zero-value of the field
// so we can verify hotReload actually wrote something.
var hotReloadTestValues = map[string]hotReloadTestValue{
	// --- collector ---
	"collector.interval_seconds": {
		input:  "42",
		reader: func(c *config.Config) string { return itoa(c.Collector.IntervalSeconds) },
	},
	"collector.batch_size": {
		input:  "99",
		reader: func(c *config.Config) string { return itoa(c.Collector.BatchSize) },
	},
	"collector.max_queries": {
		input:  "77",
		reader: func(c *config.Config) string { return itoa(c.Collector.MaxQueries) },
	},

	// --- analyzer ---
	"analyzer.interval_seconds": {
		input:  "120",
		reader: func(c *config.Config) string { return itoa(c.Analyzer.IntervalSeconds) },
	},
	"analyzer.slow_query_threshold_ms": {
		input:  "500",
		reader: func(c *config.Config) string { return itoa(c.Analyzer.SlowQueryThresholdMs) },
	},
	"analyzer.seq_scan_min_rows": {
		input:  "5000",
		reader: func(c *config.Config) string { return itoa(c.Analyzer.SeqScanMinRows) },
	},
	"analyzer.unused_index_window_days": {
		input:  "14",
		reader: func(c *config.Config) string { return itoa(c.Analyzer.UnusedIndexWindowDays) },
	},
	"analyzer.index_bloat_threshold_pct": {
		input:  "30",
		reader: func(c *config.Config) string { return itoa(c.Analyzer.IndexBloatThresholdPct) },
	},
	"analyzer.table_bloat_dead_tuple_pct": {
		input:  "25",
		reader: func(c *config.Config) string { return itoa(c.Analyzer.TableBloatDeadTuplePct) },
	},
	"analyzer.regression_threshold_pct": {
		input:  "40",
		reader: func(c *config.Config) string { return itoa(c.Analyzer.RegressionThresholdPct) },
	},
	"analyzer.cache_hit_ratio_warning": {
		input:  "0.85",
		reader: func(c *config.Config) string { return ftoa(c.Analyzer.CacheHitRatioWarning) },
	},

	// --- trust ---
	"trust.level": {
		input:  "advisory",
		reader: func(c *config.Config) string { return c.Trust.Level },
	},
	"trust.tier3_safe": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.Trust.Tier3Safe) },
	},
	"trust.tier3_moderate": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.Trust.Tier3Moderate) },
	},
	"trust.tier3_high_risk": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.Trust.Tier3HighRisk) },
	},
	"trust.maintenance_window": {
		input:  "Sun 02:00-06:00",
		reader: func(c *config.Config) string { return c.Trust.MaintenanceWindow },
	},
	"trust.rollback_threshold_pct": {
		input:  "15",
		reader: func(c *config.Config) string { return itoa(c.Trust.RollbackThresholdPct) },
	},
	"trust.rollback_window_minutes": {
		input:  "30",
		reader: func(c *config.Config) string { return itoa(c.Trust.RollbackWindowMinutes) },
	},
	"trust.rollback_cooldown_days": {
		input:  "5",
		reader: func(c *config.Config) string { return itoa(c.Trust.RollbackCooldownDays) },
	},
	"trust.cascade_cooldown_cycles": {
		input:  "3",
		reader: func(c *config.Config) string { return itoa(c.Trust.CascadeCooldownCycles) },
	},

	// --- safety ---
	"safety.cpu_ceiling_pct": {
		input:  "90",
		reader: func(c *config.Config) string { return itoa(c.Safety.CPUCeilingPct) },
	},
	"safety.query_timeout_ms": {
		input:  "10000",
		reader: func(c *config.Config) string { return itoa(c.Safety.QueryTimeoutMs) },
	},
	"safety.ddl_timeout_seconds": {
		input:  "60",
		reader: func(c *config.Config) string { return itoa(c.Safety.DDLTimeoutSeconds) },
	},
	"safety.lock_timeout_ms": {
		input:  "2000",
		reader: func(c *config.Config) string { return itoa(c.Safety.LockTimeoutMs) },
	},

	// --- llm ---
	"llm.enabled": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.LLM.Enabled) },
	},
	"llm.endpoint": {
		input:  "https://api.example.com/v1",
		reader: func(c *config.Config) string { return c.LLM.Endpoint },
	},
	"llm.api_key": {
		input:  "sk-test-key-12345",
		reader: func(c *config.Config) string { return c.LLM.APIKey },
	},
	"llm.model": {
		input:  "gpt-4o",
		reader: func(c *config.Config) string { return c.LLM.Model },
	},
	"llm.json_mode": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.LLM.JSONMode) },
	},
	"llm.timeout_seconds": {
		input:  "45",
		reader: func(c *config.Config) string { return itoa(c.LLM.TimeoutSeconds) },
	},
	"llm.token_budget_daily": {
		input:  "200000",
		reader: func(c *config.Config) string { return itoa(c.LLM.TokenBudgetDaily) },
	},
	"llm.context_budget_tokens": {
		input:  "8192",
		reader: func(c *config.Config) string { return itoa(c.LLM.ContextBudgetTokens) },
	},
	"llm.optimizer.enabled": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.LLM.Optimizer.Enabled) },
	},
	"llm.optimizer.min_query_calls": {
		input:  "5",
		reader: func(c *config.Config) string { return itoa(c.LLM.Optimizer.MinQueryCalls) },
	},
	"llm.optimizer.max_new_per_table": {
		input:  "7",
		reader: func(c *config.Config) string { return itoa(c.LLM.Optimizer.MaxNewPerTable) },
	},

	// --- advisor ---
	"advisor.enabled": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.Advisor.Enabled) },
	},
	"advisor.interval_seconds": {
		input:  "120",
		reader: func(c *config.Config) string { return itoa(c.Advisor.IntervalSeconds) },
	},

	// --- alerting ---
	"alerting.enabled": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.Alerting.Enabled) },
	},
	"alerting.slack_webhook_url": {
		input:  "https://hooks.slack.com/test",
		reader: func(c *config.Config) string { return c.Alerting.SlackWebhookURL },
	},
	"alerting.pagerduty_routing_key": {
		input:  "pd-routing-key-abc",
		reader: func(c *config.Config) string { return c.Alerting.PagerDutyRoutingKey },
	},
	"alerting.check_interval_seconds": {
		input:  "60",
		reader: func(c *config.Config) string { return itoa(c.Alerting.CheckIntervalSeconds) },
	},
	"alerting.cooldown_minutes": {
		input:  "30",
		reader: func(c *config.Config) string { return itoa(c.Alerting.CooldownMinutes) },
	},
	"alerting.quiet_hours_start": {
		input:  "23:00",
		reader: func(c *config.Config) string { return c.Alerting.QuietHoursStart },
	},
	"alerting.quiet_hours_end": {
		input:  "07:00",
		reader: func(c *config.Config) string { return c.Alerting.QuietHoursEnd },
	},

	// --- retention ---
	"retention.snapshots_days": {
		input:  "14",
		reader: func(c *config.Config) string { return itoa(c.Retention.SnapshotsDays) },
	},
	"retention.findings_days": {
		input:  "60",
		reader: func(c *config.Config) string { return itoa(c.Retention.FindingsDays) },
	},
	"retention.actions_days": {
		input:  "180",
		reader: func(c *config.Config) string { return itoa(c.Retention.ActionsDays) },
	},
	"retention.explains_days": {
		input:  "30",
		reader: func(c *config.Config) string { return itoa(c.Retention.ExplainsDays) },
	},

	// --- v0.9.2: slow active replication slot threshold ---
	"analyzer.slow_slot_retained_bytes": {
		input:  "1073741824",
		reader: func(c *config.Config) string { return fmt.Sprintf("%d", c.Analyzer.SlowSlotRetainedBytes) },
	},

	// --- v0.9: lock chain detection ---
	"analyzer.lock_chain.enabled": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.Analyzer.LockChain.Enabled) },
	},
	"analyzer.lock_chain.min_blocked_threshold": {
		input:  "42",
		reader: func(c *config.Config) string { return itoa(c.Analyzer.LockChain.MinBlockedThreshold) },
	},
	"analyzer.lock_chain.critical_blocked_threshold": {
		input:  "42",
		reader: func(c *config.Config) string { return itoa(c.Analyzer.LockChain.CriticalBlockedThreshold) },
	},
	"analyzer.lock_chain.idle_in_tx_terminate_minutes": {
		input:  "42",
		reader: func(c *config.Config) string { return itoa(c.Analyzer.LockChain.IdleInTxTerminateMinutes) },
	},
	"analyzer.lock_chain.active_query_cancel_minutes": {
		input:  "42",
		reader: func(c *config.Config) string { return itoa(c.Analyzer.LockChain.ActiveQueryCancelMinutes) },
	},

	// --- v0.9: RCA engine ---
	"rca.enabled": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.RCA.Enabled) },
	},
	"rca.llm_correlation_threshold": {
		input:  "42",
		reader: func(c *config.Config) string { return itoa(c.RCA.LLMCorrelationThreshold) },
	},
	"rca.dedup_window_minutes": {
		input:  "42",
		reader: func(c *config.Config) string { return itoa(c.RCA.DedupWindowMinutes) },
	},
	"rca.escalation_cycles": {
		input:  "42",
		reader: func(c *config.Config) string { return itoa(c.RCA.EscalationCycles) },
	},
	"rca.resolution_cycles": {
		input:  "42",
		reader: func(c *config.Config) string { return itoa(c.RCA.ResolutionCycles) },
	},
	"rca.connection_saturation_pct": {
		input:  "80",
		reader: func(c *config.Config) string { return itoa(c.RCA.ConnectionSaturationPct) },
	},
	"rca.replication_lag_threshold_seconds": {
		input:  "42",
		reader: func(c *config.Config) string { return itoa(c.RCA.ReplicationLagThresholdS) },
	},
	"rca.wal_spike_multiplier": {
		input:  "2.5",
		reader: func(c *config.Config) string { return ftoa(c.RCA.WALSpikeMultiplier) },
	},

	// --- v0.9: runaway query termination ---
	"runaway.enabled": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.Runaway.Enabled) },
	},

	// --- v0.9: natural language EXPLAIN ---
	"explain.enabled": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.Explain.Enabled) },
	},
	"explain.timeout_ms": {
		input:  "42",
		reader: func(c *config.Config) string { return itoa(c.Explain.TimeoutMs) },
	},
	"explain.cache_ttl_minutes": {
		input:  "42",
		reader: func(c *config.Config) string { return itoa(c.Explain.CacheTTLMinutes) },
	},
	"explain.max_tokens": {
		input:  "42",
		reader: func(c *config.Config) string { return itoa(c.Explain.MaxTokens) },
	},

	// --- v0.9.1: LogWatch (log-based RCA) ---
	"logwatch.enabled": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.LogWatch.Enabled) },
	},
	"logwatch.log_directory": {
		input:  "/var/log/postgresql",
		reader: func(c *config.Config) string { return c.LogWatch.LogDirectory },
	},
	"logwatch.format": {
		input:  "jsonlog",
		reader: func(c *config.Config) string { return c.LogWatch.Format },
	},
	"logwatch.poll_interval_ms": {
		input:  "2000",
		reader: func(c *config.Config) string { return itoa(c.LogWatch.PollIntervalMs) },
	},
	"logwatch.dedup_window_seconds": {
		input:  "120",
		reader: func(c *config.Config) string { return itoa(c.LogWatch.DedupWindowS) },
	},
	"logwatch.max_line_len_bytes": {
		input:  "32768",
		reader: func(c *config.Config) string { return itoa(c.LogWatch.MaxLineLenBytes) },
	},
	"logwatch.temp_file_min_bytes": {
		input:  "5242880",
		reader: func(c *config.Config) string { return itoa(c.LogWatch.TempFileMinBytes) },
	},
	"logwatch.max_lines_per_cycle": {
		input:  "5000",
		reader: func(c *config.Config) string { return itoa(c.LogWatch.MaxLinesPerCycle) },
	},
	"logwatch.exclude_applications": {
		input: "pg_sage",
		reader: func(c *config.Config) string {
			return strings.Join(c.LogWatch.ExcludeApplications, ",")
		},
	},
	"logwatch.slow_query_enabled": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.LogWatch.SlowQueryEnabled) },
	},

	// --- v0.10: Schema Lint ---
	"schema_lint.enabled": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.SchemaLint.Enabled) },
	},
	"schema_lint.scan_interval_minutes": {
		input:  "30",
		reader: func(c *config.Config) string { return itoa(c.SchemaLint.ScanIntervalMinutes) },
	},
	"schema_lint.min_table_rows": {
		input:  "5000",
		reader: func(c *config.Config) string { return itoa(c.SchemaLint.MinTableRows) },
	},

	// --- v0.10: Migration / DDL Safety ---
	"migration.enabled": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.Migration.Enabled) },
	},
	"migration.mode": {
		input:  "advisory",
		reader: func(c *config.Config) string { return c.Migration.Mode },
	},
	"migration.managed_service": {
		input:  "rds",
		reader: func(c *config.Config) string { return c.Migration.ManagedService },
	},
	"migration.log_detection": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.Migration.LogDetection) },
	},
	"migration.activity_polling": {
		input:  "true",
		reader: func(c *config.Config) string { return btoa(c.Migration.ActivityPolling) },
	},
	"migration.poll_interval_seconds": {
		input:  "10",
		reader: func(c *config.Config) string { return itoa(c.Migration.PollIntervalSeconds) },
	},
	"migration.ddl_row_threshold": {
		input:  "50000",
		reader: func(c *config.Config) string { return itoa(c.Migration.DDLRowThreshold) },
	},
}

// itoa formats an int for comparison.
func itoa(n int) string { return fmt.Sprintf("%d", n) }

// ftoa formats a float64 for comparison.
func ftoa(f float64) string { return fmt.Sprintf("%g", f) }

// btoa formats a bool for comparison.
func btoa(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestConfigConsistency_HotReloadDirectCoverage calls hotReload()
// for every key in allowedConfigKeys with a non-zero value, then
// verifies the corresponding config struct field was actually
// mutated. This catches three bug classes:
//
//  1. A key exists in allowedConfigKeys but hotReload has no
//     matching switch case (silently dropped).
//  2. hotReload has a case for the key but mutates the WRONG
//     field (copy-paste bug).
//  3. hotReload parses the value incorrectly (e.g. wrong type
//     conversion).
func TestConfigConsistency_HotReloadDirectCoverage(
	t *testing.T,
) {
	allowed := store.AllowedConfigKeysSnapshot()

	// Phase 1: verify the test table covers every allowed key.
	var missingFromTable []string
	for key := range allowed {
		if _, ok := hotReloadTestValues[key]; !ok {
			missingFromTable = append(missingFromTable, key)
		}
	}
	if len(missingFromTable) > 0 {
		sort.Strings(missingFromTable)
		t.Fatalf(
			"hotReloadTestValues is missing %d key(s) from "+
				"allowedConfigKeys — add them:\n  %s",
			len(missingFromTable),
			strings.Join(missingFromTable, "\n  "))
	}

	// Phase 1b: verify the test table has no stale keys absent
	// from allowedConfigKeys.
	var stale []string
	for key := range hotReloadTestValues {
		if _, ok := allowed[key]; !ok {
			stale = append(stale, key)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf(
			"hotReloadTestValues has %d key(s) not in "+
				"allowedConfigKeys — remove them:\n  %s",
			len(stale),
			strings.Join(stale, "\n  "))
	}

	// Phase 2: for each key, create a zero-value config, call
	// hotReload, and assert the field changed.
	for key, tv := range hotReloadTestValues {
		t.Run(key, func(t *testing.T) {
			cfg := &config.Config{}

			// Read the field before hotReload.
			before := tv.reader(cfg)

			// Apply the hot-reload.
			hotReload(cfg, key, tv.input)

			// Read the field after hotReload.
			after := tv.reader(cfg)

			// The field must have changed from its zero value.
			if after == before {
				t.Errorf(
					"hotReload(%q, %q) did not mutate the "+
						"config field: before=%q, after=%q",
					key, tv.input, before, after)
			}

			// The field must reflect the input value.
			// For int/float/bool, the reader returns the
			// formatted value; for strings, it returns the
			// raw value.
			if after != tv.input {
				t.Errorf(
					"hotReload(%q, %q) produced wrong value: "+
						"got %q",
					key, tv.input, after)
			}
		})
	}
}

// TestConfigConsistency_HotReloadTestTableCount is a tripwire
// that fails when hotReloadTestValues drifts from
// allowedConfigKeys. Update the expected count when adding or
// removing keys.
func TestConfigConsistency_HotReloadTestTableCount(
	t *testing.T,
) {
	allowed := store.AllowedConfigKeysSnapshot()
	tableCount := len(hotReloadTestValues)
	allowedCount := len(allowed)

	if tableCount != allowedCount {
		t.Errorf(
			"hotReloadTestValues has %d entries but "+
				"allowedConfigKeys has %d — they must match",
			tableCount, allowedCount)
	}
}
