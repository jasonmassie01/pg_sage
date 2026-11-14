package config

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// Log-watch modes of logwatch.enabled (rca-09b).
const (
	LogWatchAuto = "auto" // unset: on when a log source is detected
	LogWatchOn   = "on"   // true: always wanted; an unreadable log WARNs
	LogWatchOff  = "off"  // false: never
)

// Mode is how logwatch.enabled is set: auto (unset, the default), on or
// off.
func (c LogWatchConfig) Mode() string {
	switch {
	case c.Enabled == nil:
		return LogWatchAuto
	case *c.Enabled:
		return LogWatchOn
	default:
		return LogWatchOff
	}
}

// IsActive reports whether log RCA reads a source: always when on, never
// when off, and in auto when detected is true (a readable jsonlog or
// csvlog source, or a configured provider log sink).
func (c LogWatchConfig) IsActive(detected bool) bool {
	switch c.Mode() {
	case LogWatchOn:
		return true
	case LogWatchAuto:
		return detected
	default:
		return false
	}
}

// Setting is logwatch.enabled in the API's grammar: "auto" when unset,
// else "true" or "false".
func (c LogWatchConfig) Setting() string {
	switch c.Mode() {
	case LogWatchOn:
		return "true"
	case LogWatchOff:
		return "false"
	default:
		return LogWatchAuto
	}
}

// ParseLogWatchEnabled parses an API value of logwatch.enabled: auto
// (any case) is unset, else the API's boolean grammar.
func ParseLogWatchEnabled(value string) (*bool, error) {
	if strings.EqualFold(strings.TrimSpace(value), LogWatchAuto) {
		return nil, nil
	}
	b, err := ParseBool(value)
	if err != nil {
		return nil, fmt.Errorf("logwatch.enabled must be true, false or auto, got %q", value)
	}
	return &b, nil
}

// logWatchFields decodes the section without UnmarshalYAML.
type logWatchFields LogWatchConfig

// UnmarshalYAML reads the logwatch section. enabled takes true, false,
// auto or no value (auto); every other key decodes as usual, and an
// unknown key is refused as the strict loader would.
func (c *LogWatchConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return node.Decode((*logWatchFields)(c))
	}
	rest := *node
	rest.Content = nil
	var enabled *bool
	seen := false
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Value != "enabled" {
			rest.Content = append(rest.Content, key, value)
			continue
		}
		parsed, err := decodeLogWatchEnabled(value)
		if err != nil {
			return err
		}
		enabled, seen = parsed, true
	}
	if err := checkLogWatchKeys(rest.Content); err != nil {
		return err
	}
	if err := rest.Decode((*logWatchFields)(c)); err != nil {
		return err
	}
	if seen {
		c.Enabled = enabled
	}
	return nil
}

// decodeLogWatchEnabled reads the enabled value: null, empty or auto is
// unset; anything else must decode as a YAML boolean.
func decodeLogWatchEnabled(value *yaml.Node) (*bool, error) {
	if value.Kind == yaml.ScalarNode &&
		(value.Tag == "!!null" || strings.EqualFold(value.Value, LogWatchAuto)) {
		return nil, nil
	}
	var b bool
	if value.Kind != yaml.ScalarNode || value.Decode(&b) != nil {
		return nil, fmt.Errorf("line %d: logwatch.enabled must be true, false or auto, "+
			"got %q", value.Line, value.Value)
	}
	return &b, nil
}

// checkLogWatchKeys refuses a key that is no logwatch field, as the
// loader's KnownFields decoding does for every other section.
func checkLogWatchKeys(content []*yaml.Node) error {
	known := map[string]bool{"<<": true}
	t := reflect.TypeOf(LogWatchConfig{})
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		known[name] = true
	}
	for i := 0; i < len(content); i += 2 {
		if !known[content[i].Value] {
			return fmt.Errorf("line %d: field %s not found in type config.LogWatchConfig",
				content[i].Line, content[i].Value)
		}
	}
	return nil
}
