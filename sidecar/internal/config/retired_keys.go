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
