package decommission

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// AckSection is the YAML key of the §12 acknowledgement:
//
//	agentdb_decommission:
//	  acknowledged_resources: [<inventory item id>, ...]
//	  exported: true
const AckSection = "agentdb_decommission"

// parseAckNode reads the acknowledgement section strictly: a mapping with
// only acknowledged_resources (a list of ids) and exported (a boolean). An
// empty section is an empty request.
func parseAckNode(node *yaml.Node) (AckRequest, error) {
	var req AckRequest
	if node == nil || (node.Kind == yaml.ScalarNode && node.Tag == "!!null") {
		return req, nil
	}
	if node.Kind != yaml.MappingNode {
		return req, fmt.Errorf("%s must be a mapping with acknowledged_resources "+
			"and exported", AckSection)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch key := node.Content[i].Value; key {
		case "acknowledged_resources", "exported":
		default:
			return req, fmt.Errorf("%s: unknown key %q", AckSection, key)
		}
	}
	if err := node.Decode(&req); err != nil {
		return req, fmt.Errorf("%s: %w", AckSection, err)
	}
	return req, nil
}

// readConfigAck returns the acknowledgement section of the config file at
// path, or nil when there is no file path or no section.
func readConfigAck(path string) (*AckRequest, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s from %s: %w", AckSection, path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != AckSection {
			continue
		}
		req, err := parseAckNode(root.Content[i+1])
		if err != nil {
			return nil, err
		}
		return &req, nil
	}
	return nil, nil
}
