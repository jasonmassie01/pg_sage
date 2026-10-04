package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// perDatabaseAliases map fleet fields (defaults.* and databases[].*) to
// the canonical key they set for one database.
var perDatabaseAliases = map[string]string{
	"collector_interval_seconds": "collector.interval_seconds",
	"analyzer_interval_seconds":  "analyzer.interval_seconds",
}

// OperatorSetPaths lists the canonical key paths the operator wrote in the
// YAML document raw for the named database: every scalar or sequence leaf
// under a mapping (a section mapping itself is not a key), plus the fleet
// aliases from defaults and the database's own databases[] entry (a zero
// per-database interval falls back and is not a setting). A value equal
// to the default still counts: the operator chose it.
func OperatorSetPaths(raw []byte, database string) (map[string]bool, error) {
	out := map[string]bool{}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config YAML: %w", err)
	}
	if len(doc.Content) == 0 {
		return out, nil
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return out, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse config YAML: the document is not a mapping")
	}
	collectLeaves(root, "", out)
	addFleetAliases(root, database, out)
	return out, nil
}

// OperatorSetPathsFromFile reads the config file at path; an empty path
// (no config file) sets nothing.
func OperatorSetPathsFromFile(path, database string) (map[string]bool, error) {
	if path == "" {
		return map[string]bool{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}
	return OperatorSetPaths(raw, database)
}

func collectLeaves(node *yaml.Node, prefix string, out map[string]bool) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		switch {
		case value.Kind == yaml.MappingNode:
			collectLeaves(value, path, out)
		case value.Kind == yaml.ScalarNode && value.Tag == "!!null":
			// "section:" with nothing under it sets nothing.
		default:
			out[path] = true
		}
	}
}

func addFleetAliases(root *yaml.Node, database string, out map[string]bool) {
	if defaults := mapEntry(root, "defaults"); defaults != nil {
		addAliasFields(defaults, out)
	}
	databases := mapEntry(root, "databases")
	if databases == nil || databases.Kind != yaml.SequenceNode {
		return
	}
	for _, entry := range databases.Content {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		if name := mapEntry(entry, "name"); name != nil && name.Value == database {
			addAliasFields(entry, out)
		}
	}
}

func addAliasFields(node *yaml.Node, out map[string]bool) {
	for field, canonical := range perDatabaseAliases {
		value := mapEntry(node, field)
		if value == nil || value.Kind != yaml.ScalarNode {
			continue
		}
		if n, err := strconv.Atoi(value.Value); err == nil && n == 0 {
			continue // zero falls back to the next level
		}
		out[canonical] = true
	}
}

func mapEntry(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
