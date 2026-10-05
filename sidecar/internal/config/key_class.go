package config

import (
	_ "embed"
	"fmt"
	"strings"
	"sync"
)

// KeyClass says whether pg_sage may derive a configuration key itself
// (roadmap phase 3, self-configuration). The classification is data:
// key_classes.txt, one "<class> <key>" line per key.
type KeyClass string

const (
	// KeySafetyCritical keys are never derived: trust and approval,
	// action authority, verification that feeds trust, credentials,
	// endpoints, LLM provider and keys, and the self_config switch itself.
	KeySafetyCritical KeyClass = "safety_critical"
	// KeyOperatorPreference keys are never derived: what the operator
	// declares (notification routes, windows, capacities, feature switches).
	KeyOperatorPreference KeyClass = "operator_preference"
	// KeyDerivable keys may be derived per database from evidence, within
	// bounds that never widen authority or spend, when the operator leaves
	// them unset.
	KeyDerivable KeyClass = "derivable"
)

//go:embed key_classes.txt
var keyClassesData string

var (
	keyClassesOnce sync.Once
	keyClasses     map[string]KeyClass
)

func loadKeyClasses() map[string]KeyClass {
	keyClassesOnce.Do(func() {
		parsed, err := parseKeyClasses(keyClassesData)
		if err != nil {
			// The embedded file is checked by tests; a broken one must not
			// silently classify keys as derivable.
			panic("config: key_classes.txt: " + err.Error())
		}
		keyClasses = parsed
	})
	return keyClasses
}

// KeyClassOf returns the self-configuration class of a key path (as in
// FieldLifecycles and DatabaseFieldLifecycles); false when unclassified.
func KeyClassOf(path string) (KeyClass, bool) {
	class, ok := loadKeyClasses()[path]
	return class, ok
}

// KeyClassification returns a copy of the whole classification.
func KeyClassification() map[string]KeyClass {
	src := loadKeyClasses()
	out := make(map[string]KeyClass, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func parseKeyClasses(data string) (map[string]KeyClass, error) {
	out := map[string]KeyClass{}
	for i, line := range strings.Split(data, "\n") {
		if cut := strings.Index(line, "#"); cut >= 0 {
			line = line[:cut]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want \"<class> <key>\", got %q", i+1, line)
		}
		class, key := KeyClass(fields[0]), fields[1]
		switch class {
		case KeySafetyCritical, KeyOperatorPreference, KeyDerivable:
		default:
			return nil, fmt.Errorf("line %d: unknown class %q", i+1, class)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("line %d: %s classified twice", i+1, key)
		}
		out[key] = class
	}
	return out, nil
}
