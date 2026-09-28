package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// rejectRetiredTunerKeys names tuner keys that were removed because they
// never had an effect, so an old config fails with an instruction instead
// of a bare "field not found".
func rejectRetiredTunerKeys(tuner *yaml.Node) error {
	if tuner == nil || tuner.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(tuner.Content); i += 2 {
		if tuner.Content[i].Value == "analyze_maintenance_threshold_mb" {
			return fmt.Errorf("configuration key %q was removed: it never had an "+
				"effect; remove it from the config file",
				"tuner.analyze_maintenance_threshold_mb")
		}
	}
	return nil
}
