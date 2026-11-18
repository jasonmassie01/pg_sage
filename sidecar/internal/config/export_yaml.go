package config

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// EffectiveYAML backs `pg_sage_sidecar config export` and the Settings
// page's "equivalent YAML" (cli-01b): the YAML a config file would hold
// to set keys to their values in cfg, nested as the file nests them. A
// secret never prints: its value is the ${NAME} placeholder the loader
// expands from the environment, whether or not it is set.

// secretKeyRe names credential-shaped keys. EffectiveYAML treats them as
// secret even when the caller's predicate does not (fail closed).
var secretKeyRe = regexp.MustCompile(
	`(api_key|password|routing_key|secret|token|encryption_key|webhook_url|dsn)$`)

// LooksSecretKey reports a key whose name says it holds a credential.
func LooksSecretKey(key string) bool {
	return secretKeyRe.MatchString(key)
}

// SecretPlaceholder is the ${NAME} reference written in place of a
// secret key: the variable the loader reads for it when there is one
// (llm.api_key is SAGE_LLM_API_KEY), else SAGE_ and the key in capitals.
func SecretPlaceholder(key string) string {
	names := make([]string, 0, 1)
	for name, path := range envExplicitPaths {
		if path == key {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		names = append(names, "SAGE_"+strings.ToUpper(
			strings.NewReplacer(".", "_", "-", "_").Replace(key)))
	}
	sort.Strings(names)
	return "${" + names[0] + "}"
}

// EffectiveYAML renders keys (dotted, in the order given) of cfg. No keys
// is "{}". secret may be nil; LooksSecretKey applies either way.
func EffectiveYAML(cfg *Config, keys []string, secret func(string) bool) ([]byte, error) {
	if cfg == nil {
		return nil, errors.New("render config YAML: config is nil")
	}
	if len(keys) == 0 {
		return []byte("{}\n"), nil
	}
	var full yaml.Node
	if err := full.Encode(cfg); err != nil {
		return nil, fmt.Errorf("render config YAML: encode the config: %w", err)
	}
	root := &full
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, key := range keys {
		parts := strings.Split(key, ".")
		leaf, ok := nodeAtPath(root, parts)
		if !ok {
			return nil, fmt.Errorf("render config YAML: %q is not a config key", key)
		}
		if LooksSecretKey(key) || (secret != nil && secret(key)) {
			leaf = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str",
				Value: SecretPlaceholder(key)}
		} else if hasSecretKey(leaf, key) {
			return nil, fmt.Errorf("render config YAML: %q holds a secret key", key)
		}
		setNodeAtPath(out, parts, leaf)
	}
	return yaml.Marshal(out)
}

// nodeAtPath returns the value node at parts under mapping node.
func nodeAtPath(node *yaml.Node, parts []string) (*yaml.Node, bool) {
	for _, part := range parts {
		if node.Kind != yaml.MappingNode {
			return nil, false
		}
		next := childValue(node, part)
		if next == nil {
			return nil, false
		}
		node = next
	}
	return node, true
}

// setNodeAtPath sets value at parts, creating the mappings on the way.
func setNodeAtPath(node *yaml.Node, parts []string, value *yaml.Node) {
	for _, part := range parts[:len(parts)-1] {
		next := childValue(node, part)
		if next == nil || next.Kind != yaml.MappingNode {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			node.Content = append(node.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, next)
		}
		node = next
	}
	last := parts[len(parts)-1]
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == last {
			node.Content[i+1] = value
			return
		}
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: last}, value)
}

// hasSecretKey reports a mapping under prefix that holds a
// credential-shaped key: a section is never exported whole with one.
func hasSecretKey(node *yaml.Node, prefix string) bool {
	if node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := joinKey(prefix, node.Content[i].Value)
		if LooksSecretKey(key) || hasSecretKey(node.Content[i+1], key) {
			return true
		}
	}
	return false
}
