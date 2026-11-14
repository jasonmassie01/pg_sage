package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// SREInvestigatorConfig is sre.investigator (sre-03 Phase 2). With
// RequireSupportCitation on, a model contest or conclusion keeps its
// root only when a kept claim cites evidence the causal graph records as
// support for that root; otherwise it is inconclusive (unsupported_root).
// Restart-scoped.
type SREInvestigatorConfig struct {
	RequireSupportCitation bool `yaml:"require_support_citation" doc:"A model contest or conclusion needs a kept claim citing evidence the causal graph records as support for its root, else it is inconclusive (unsupported_root). Default: true."`
}

const sreInvestigatorKey = "sre.investigator"

// UnmarshalYAML decodes the block strictly, naming the key a bad value or
// an unknown field belongs to (yaml.v3 reports only the line).
func (c *SREInvestigatorConfig) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("%s must be a mapping (line %d)", sreInvestigatorKey, n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		if key.Value != "require_support_citation" {
			return fmt.Errorf("%s: field %s not found (line %d)", sreInvestigatorKey,
				key.Value, key.Line)
		}
		var b bool
		if val.Kind != yaml.ScalarNode || val.ShortTag() != "!!bool" || val.Decode(&b) != nil {
			return fmt.Errorf("%s.require_support_citation must be true or false, got %q "+
				"(line %d)", sreInvestigatorKey, val.Value, val.Line)
		}
		c.RequireSupportCitation = b
	}
	return nil
}
