package config

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// RetiredSection is a top-level config key that an earlier release
// understood and this one does not. The package that owns the removal
// registers it, so a config file that still carries it keeps loading.
type RetiredSection struct {
	Key string
	// Check sees the section's value node before it is dropped. Its
	// warnings are printed; an error refuses the whole config.
	Check func(node *yaml.Node) (warnings []string, err error)
}

var (
	retiredSectionsMu sync.RWMutex
	retiredSections   = map[string]RetiredSection{}
)

// RegisterRetiredSection registers s, normally from the owning package's
// init function. It panics on an empty key, a nil Check, a key the Config
// struct still decodes, or a second registration of the same key.
func RegisterRetiredSection(s RetiredSection) {
	key := strings.TrimSpace(s.Key)
	switch {
	case key == "" || key != s.Key:
		panic(fmt.Sprintf("config: invalid retired section key %q", s.Key))
	case s.Check == nil:
		panic(fmt.Sprintf("config: retired section %q has no Check", key))
	case liveTopLevelKey(key):
		panic(fmt.Sprintf("config: %q is a live config key, not a retired one", key))
	}
	retiredSectionsMu.Lock()
	defer retiredSectionsMu.Unlock()
	if _, dup := retiredSections[key]; dup {
		panic(fmt.Sprintf("config: retired section %q registered twice", key))
	}
	retiredSections[key] = s
}

// liveTopLevelKey reports whether the Config struct decodes key.
func liveTopLevelKey(key string) bool {
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name == key {
			return true
		}
	}
	return false
}

// stripRetiredSections runs each registered section's Check and removes the
// section from raw. raw is returned unchanged when it carries none.
func stripRetiredSections(raw string) (string, []string, error) {
	retiredSectionsMu.RLock()
	defer retiredSectionsMu.RUnlock()
	if len(retiredSections) == 0 {
		return raw, nil, nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		return "", nil, err
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return raw, nil, nil
	}
	root := document.Content[0]
	var warnings []string
	kept := make([]*yaml.Node, 0, len(root.Content))
	for i := 0; i+1 < len(root.Content); i += 2 {
		section, retired := retiredSections[root.Content[i].Value]
		if !retired {
			kept = append(kept, root.Content[i], root.Content[i+1])
			continue
		}
		w, err := section.Check(root.Content[i+1])
		if err != nil {
			return "", nil, fmt.Errorf("config section %q: %w", section.Key, err)
		}
		warnings = append(warnings, w...)
	}
	if len(kept) == len(root.Content) {
		return raw, warnings, nil
	}
	root.Content = kept
	stripped, err := yaml.Marshal(&document)
	if err != nil {
		return "", nil, fmt.Errorf("drop retired config sections: %w", err)
	}
	return string(stripped), warnings, nil
}

// dropRetired removes retired tuner keys and retired sections from raw and
// prints their warnings.
func dropRetired(raw string) (string, error) {
	raw, keyWarnings, err := stripRetiredKeys(raw)
	if err != nil {
		return "", err
	}
	raw, sectionWarnings, err := stripRetiredSections(raw)
	if err != nil {
		return "", err
	}
	for _, warning := range append(keyWarnings, sectionWarnings...) {
		_, _ = fmt.Fprintln(configWarningOutput, warning)
	}
	return raw, nil
}
