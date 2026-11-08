package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// retiredTunerKeys are tuner keys that were removed because they never had
// an effect. A config that still sets one loads: the key is dropped before
// the strict decode and a warning tells the operator to remove it.
var retiredTunerKeys = map[string]string{
	"analyze_maintenance_threshold_mb": "tuner.analyze_maintenance_threshold_mb " +
		"is no longer used and is ignored; remove it from the config file",
	// tuning-09: revalidation does no hinted-vs-unhinted cost comparison and
	// issues no EXPLAIN, so these three never had a reader.
	"revalidation_keep_ratio": "tuner.revalidation_keep_ratio is no longer used " +
		"and is ignored; remove it from the config file (revalidation compares " +
		"no plan costs)",
	"revalidation_rollback_ratio": "tuner.revalidation_rollback_ratio is no longer " +
		"used and is ignored; remove it from the config file (revalidation never " +
		"rolls a hint back on a cost ratio)",
	"revalidation_explain_timeout_ms": "tuner.revalidation_explain_timeout_ms is no " +
		"longer used and is ignored; remove it from the config file (revalidation " +
		"runs no EXPLAIN)",
}

// stripRetiredKeys removes retired keys from raw and returns the YAML to
// decode plus one warning per removed key. raw is returned unchanged when
// it sets none of them.
func stripRetiredKeys(raw string) (string, []string, error) {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		return "", nil, err
	}
	tuner := mappingValue(&document, "tuner")
	if tuner == nil || tuner.Kind != yaml.MappingNode {
		return raw, nil, nil
	}
	var warnings []string
	kept := tuner.Content[:0]
	for i := 0; i+1 < len(tuner.Content); i += 2 {
		if warning, retired := retiredTunerKeys[tuner.Content[i].Value]; retired {
			warnings = append(warnings, "WARNING: "+warning)
			continue
		}
		kept = append(kept, tuner.Content[i], tuner.Content[i+1])
	}
	if len(warnings) == 0 {
		return raw, nil, nil
	}
	tuner.Content = kept
	stripped, err := yaml.Marshal(&document)
	if err != nil {
		return "", nil, fmt.Errorf("drop retired config keys: %w", err)
	}
	return string(stripped), warnings, nil
}

// ignoredOptimizerKeys are llm.optimizer keys that still decode but have no
// reader; indexes-11 retires them (stripRetiredKeys).
var ignoredOptimizerKeys = map[string]string{
	"plan_source": "llm.optimizer.plan_source is no longer used and is ignored: the " +
		"optimizer captures no plans (the HypoPG what-if plans candidates, bounded); " +
		"remove it from the config file",
}

// ignoredKeyWarnings returns one warning per ignored key raw sets.
func ignoredKeyWarnings(raw string) ([]string, error) {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		return nil, fmt.Errorf("check ignored config keys: %w", err)
	}
	optimizer := childValue(mappingValue(&document, "llm"), "optimizer")
	if optimizer == nil || optimizer.Kind != yaml.MappingNode {
		return nil, nil
	}
	var warnings []string
	for i := 0; i+1 < len(optimizer.Content); i += 2 {
		if warning, ignored := ignoredOptimizerKeys[optimizer.Content[i].Value]; ignored {
			warnings = append(warnings, "WARNING: "+warning)
		}
	}
	return warnings, nil
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
	root := document.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return root.Content[i+1]
		}
	}
	return nil
}
