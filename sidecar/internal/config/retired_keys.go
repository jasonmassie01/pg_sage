package config

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// retiredKeys are keys of a top-level section that were removed: the tuner
// key never had an effect, and the forecaster's sequence thresholds went
// with its sequence rule (forecast-05: the runway forecasts sequences,
// sre.runways.sequence_horizon_days and sequence_critical_days). A config
// that still sets one loads: the key is dropped before the strict decode
// and a warning tells the operator to remove it.
var retiredKeys = map[string]map[string]string{
	"tuner": {
		"analyze_maintenance_threshold_mb": "tuner.analyze_maintenance_threshold_mb " +
			"is no longer used and is ignored; remove it from the config file",
	},
	"forecaster": {
		"sequence_warn_days": retiredSequenceKey("sequence_warn_days",
			"sequence_horizon_days"),
		"sequence_critical_days": retiredSequenceKey("sequence_critical_days",
			"sequence_critical_days"),
	},
}

func retiredSequenceKey(key, replacement string) string {
	return "forecaster." + key + " is no longer used and is ignored: sequence " +
		"exhaustion is forecast by the runway monitor (sre.runways." + replacement +
		"); remove it from the config file"
}

// stripRetiredKeys removes retired keys from raw and returns the YAML to
// decode plus one warning per removed key. raw is returned unchanged when
// it sets none of them.
func stripRetiredKeys(raw string) (string, []string, error) {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		return "", nil, err
	}
	var warnings []string
	for section, keys := range retiredKeys {
		warnings = append(warnings, dropKeys(mappingValue(&document, section), keys)...)
	}
	if len(warnings) == 0 {
		return raw, nil, nil
	}
	sort.Strings(warnings)
	stripped, err := yaml.Marshal(&document)
	if err != nil {
		return "", nil, fmt.Errorf("drop retired config keys: %w", err)
	}
	return string(stripped), warnings, nil
}

// dropKeys removes the retired keys from the mapping section and returns a
// warning for each.
func dropKeys(section *yaml.Node, retired map[string]string) []string {
	if section == nil || section.Kind != yaml.MappingNode {
		return nil
	}
	var warnings []string
	kept := section.Content[:0]
	for i := 0; i+1 < len(section.Content); i += 2 {
		if warning, ok := retired[section.Content[i].Value]; ok {
			warnings = append(warnings, "WARNING: "+warning)
			continue
		}
		kept = append(kept, section.Content[i], section.Content[i+1])
	}
	section.Content = kept
	return warnings
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
