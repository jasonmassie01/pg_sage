package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// retiredKeys are keys that were removed because nothing read them (or
// their rule was retired), by the mapping that held them. A config that
// still sets one loads: the key is dropped before the strict decode and a
// warning tells the operator to remove it.
var retiredKeys = []struct {
	path []string
	keys map[string]string
}{
	{[]string{"tuner"}, map[string]string{
		"analyze_maintenance_threshold_mb": "tuner.analyze_maintenance_threshold_mb " +
			"is no longer used and is ignored; remove it from the config file",
		// tuning-09: revalidation does no hinted-vs-unhinted cost comparison
		// and issues no EXPLAIN, so these three never had a reader.
		"revalidation_keep_ratio": "tuner.revalidation_keep_ratio is no longer used " +
			"and is ignored; remove it from the config file (revalidation compares " +
			"no plan costs)",
		"revalidation_rollback_ratio": "tuner.revalidation_rollback_ratio is no " +
			"longer used and is ignored; remove it from the config file (revalidation " +
			"never rolls a hint back on a cost ratio)",
		"revalidation_explain_timeout_ms": "tuner.revalidation_explain_timeout_ms is " +
			"no longer used and is ignored; remove it from the config file " +
			"(revalidation runs no EXPLAIN)",
	}},
	// vacuum-08: the analyzer's absolute freeze age gave way to a share of
	// each table's effective freeze maximum.
	{[]string{"analyzer"}, map[string]string{
		"wraparound_freeze_xid_age": "analyzer.wraparound_freeze_xid_age " +
			"is retired and ignored: the analyzer now reports wraparound at " +
			"analyzer.wraparound_freeze_share of each table's effective freeze maximum " +
			"(default 0.75) and the freeze custodian decides the VACUUM; remove it",
	}},
	// migration-12: nothing ever read it; pg_sage now asks the server
	// whether pg_repack is available.
	{[]string{"migration"}, map[string]string{
		"managed_service": "migration.managed_service is no longer used and is " +
			"ignored: pg_sage now checks whether the server offers pg_repack " +
			"before recommending it; remove it from the config file",
	}},
	// forecast-05: the forecaster's sequence thresholds went with its
	// sequence rule; the runway forecasts sequences
	// (sre.runways.sequence_horizon_days and sequence_critical_days).
	// forecast-06: min_data_points and alert_horizons fed only the v0.9
	// growth forecaster, which never ran and is gone.
	{[]string{"forecaster"}, map[string]string{
		"sequence_warn_days": retiredSequenceKey("sequence_warn_days",
			"sequence_horizon_days"),
		"sequence_critical_days": retiredSequenceKey("sequence_critical_days",
			"sequence_critical_days"),
		"min_data_points": retiredGrowthKey("min_data_points"),
		"alert_horizons":  retiredGrowthKey("alert_horizons"),
	}},
	{[]string{"llm", "optimizer"}, map[string]string{
		"plan_source": "llm.optimizer.plan_source is no longer used and is ignored: the " +
			"optimizer captures no plans (the HypoPG what-if plans candidates, bounded); " +
			"remove it from the config file",
		"over_indexed_ratio_pct": "llm.optimizer.over_indexed_ratio_pct is no longer " +
			"used and is ignored; remove it from the config file",
	}},
	// executor-04: a fleet canary waits for each database's own
	// verification verdict, not a fixed settle.
	{[]string{"sre", "autonomy", "canary"}, map[string]string{
		"settle_seconds": "sre.autonomy.canary.settle_seconds is no longer used and is " +
			"ignored: a fleet canary waits for each database's own verification verdict; " +
			"remove it from the config file",
	}},
	// rca-04: nothing read it; the on-demand EXPLAIN is never logged by
	// auto_explain, so loading the module for it did nothing.
	{[]string{"auto_explain"}, map[string]string{
		"prefer_session_load": "auto_explain.prefer_session_load is no longer used and " +
			"is ignored: pg_sage loads no auto_explain settings into its own EXPLAIN " +
			"(auto_explain never logs an EXPLAIN); remove it from the config file",
	}},
	{[]string{"llm", "index_optimizer"}, map[string]string{
		"over_indexed_ratio_pct": "llm.index_optimizer.over_indexed_ratio_pct is no " +
			"longer used and is ignored; remove it from the config file",
	}},
}

func retiredSequenceKey(key, replacement string) string {
	return "forecaster." + key + " is no longer used and is ignored: sequence " +
		"exhaustion is forecast by the runway monitor (sre.runways." + replacement +
		"); remove it from the config file"
}

func retiredGrowthKey(key string) string {
	return "forecaster." + key + " has no effect and is ignored: disk growth is " +
		"forecast by forecast_disk_growth (days_until_full against " +
		"forecaster.disk_capacity_bytes or the provider capacity) and the disk runway; " +
		"remove it from the config file"
}

// stripRetiredKeys removes retired keys from raw and returns the YAML to
// decode plus one warning per removed key. raw is returned unchanged when
// it sets none of them.
func stripRetiredKeys(raw string) (string, []string, error) {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		return "", nil, fmt.Errorf("read config to drop retired keys: %w", err)
	}
	var warnings []string
	for _, retired := range retiredKeys {
		warnings = append(warnings, dropKeys(nodeAt(&document, retired.path),
			retired.keys)...)
	}
	if len(warnings) == 0 {
		return raw, nil, nil
	}
	stripped, err := yaml.Marshal(&document)
	if err != nil {
		return "", nil, fmt.Errorf("drop retired config keys: %w", err)
	}
	return string(stripped), warnings, nil
}

// dropKeys removes the keys of retired from mapping (a no-op unless it is
// a mapping) and returns one warning per key removed, in file order.
func dropKeys(mapping *yaml.Node, retired map[string]string) []string {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	var warnings []string
	kept := mapping.Content[:0]
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if warning, ok := retired[mapping.Content[i].Value]; ok {
			warnings = append(warnings, "WARNING: "+warning)
			continue
		}
		kept = append(kept, mapping.Content[i], mapping.Content[i+1])
	}
	mapping.Content = kept
	return warnings
}

// nodeAt returns the value node at path from the document root (nil when
// any step is absent or not a mapping).
func nodeAt(document *yaml.Node, path []string) *yaml.Node {
	node := mappingValue(document, path[0])
	for _, key := range path[1:] {
		node = childValue(node, key)
	}
	return node
}

// childValue returns the value node of key in mapping (nil when absent or
// when mapping is not a mapping).
func childValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// mappingValue returns the value node of key in the document's root mapping.
func mappingValue(document *yaml.Node, key string) *yaml.Node {
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	return childValue(document.Content[0], key)
}
