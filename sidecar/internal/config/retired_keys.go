package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// retiredKeys are keys that were removed because nothing read them, by
// the mapping that held them. A config that still sets one loads: the key
// is dropped before the strict decode and a warning tells the operator to
// remove it.
var retiredKeys = []struct {
	path []string
	keys map[string]string
}{
	{[]string{"tuner"}, map[string]string{
		"analyze_maintenance_threshold_mb": "tuner.analyze_maintenance_threshold_mb " +
			"is no longer used and is ignored; remove it from the config file",
	}},
	{[]string{"llm", "optimizer"}, map[string]string{
		"plan_source": "llm.optimizer.plan_source is no longer used and is ignored: the " +
			"optimizer captures no plans (the HypoPG what-if plans candidates, bounded); " +
			"remove it from the config file",
		"over_indexed_ratio_pct": "llm.optimizer.over_indexed_ratio_pct is no longer " +
			"used and is ignored; remove it from the config file",
	}},
	{[]string{"llm", "index_optimizer"}, map[string]string{
		"over_indexed_ratio_pct": "llm.index_optimizer.over_indexed_ratio_pct is no " +
			"longer used and is ignored; remove it from the config file",
	}},
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
