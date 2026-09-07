package config

import (
	"reflect"
	"sort"
	"strings"
	"sync"
)

// ConfigLifecycle describes how a configuration field can become effective.
type ConfigLifecycle string

const (
	LifecycleLivePolicy  ConfigLifecycle = "live_policy"
	LifecycleReconfigure ConfigLifecycle = "reconfigure"
	LifecycleRestart     ConfigLifecycle = "restart"
	LifecycleAPI         ConfigLifecycle = "lifecycle_api"
)

// FieldLifecycle is the canonical lifecycle metadata for one YAML field.
type FieldLifecycle struct {
	Path      string
	Lifecycle ConfigLifecycle
	Owner     string
}

type lifecycleEntry struct {
	FieldLifecycle
	index []int
}

var (
	lifecycleOnce   sync.Once
	lifecycleByPath map[string]lifecycleEntry
	lifecyclePaths  []string
)

// LookupFieldLifecycle returns lifecycle metadata only for real YAML fields.
func LookupFieldLifecycle(path string) (FieldLifecycle, bool) {
	initializeLifecycleRegistry()
	entry, ok := lifecycleByPath[path]
	return entry.FieldLifecycle, ok
}

// FieldLifecycles returns a stable copy of the complete typed registry.
func FieldLifecycles() []FieldLifecycle {
	initializeLifecycleRegistry()
	fields := make([]FieldLifecycle, 0, len(lifecyclePaths))
	for _, path := range lifecyclePaths {
		fields = append(fields, lifecycleByPath[path].FieldLifecycle)
	}
	return fields
}

func initializeLifecycleRegistry() {
	lifecycleOnce.Do(func() {
		lifecycleByPath = make(map[string]lifecycleEntry)
		walkConfigFields(reflect.TypeOf(Config{}), nil, "")
		lifecyclePaths = make([]string, 0, len(lifecycleByPath))
		for path := range lifecycleByPath {
			lifecyclePaths = append(lifecyclePaths, path)
		}
		sort.Strings(lifecyclePaths)
	})
}

func walkConfigFields(configType reflect.Type, parentIndex []int, prefix string) {
	for i := 0; i < configType.NumField(); i++ {
		field := configType.Field(i)
		yamlName := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if yamlName == "" || yamlName == "-" {
			continue
		}
		path := yamlName
		if prefix != "" {
			path = prefix + "." + yamlName
		}
		index := append(append([]int(nil), parentIndex...), i)
		if field.Type.Kind() == reflect.Struct {
			walkConfigFields(field.Type, index, path)
			continue
		}
		lifecycle, owner := classifyField(path)
		lifecycleByPath[path] = lifecycleEntry{
			FieldLifecycle: FieldLifecycle{Path: path, Lifecycle: lifecycle, Owner: owner},
			index:          index,
		}
	}
}

func classifyField(path string) (ConfigLifecycle, string) {
	if path == "databases" {
		return LifecycleAPI, ""
	}
	if isRestartField(path) {
		return LifecycleRestart, ""
	}
	if owner := reconfigurationOwner(path); owner != "" {
		return LifecycleReconfigure, owner
	}
	if path == "trust.level" {
		return LifecycleLivePolicy, "trust_policy"
	}
	// A field is restart-bound until a concrete runtime consumer proves it can
	// adopt an immutable snapshot or supplies a reconfiguration owner.
	return LifecycleRestart, ""
}

func isRestartField(path string) bool {
	if path == "mode" || path == "meta_db" || path == "encryption_key" {
		return true
	}
	if strings.HasPrefix(path, "postgres.") || strings.HasPrefix(path, "oauth.") {
		return true
	}
	return path == "api.listen_addr" || path == "api.trusted_proxies" ||
		path == "prometheus.listen_addr"
}

func reconfigurationOwner(path string) string {
	exact := map[string]string{
		"collector.interval_seconds":            "collector",
		"analyzer.interval_seconds":             "analyzer",
		"agentdb.reconcile_interval_seconds":    "agentdb",
		"tuner.revalidation_interval_hours":     "tuner",
		"schema_lint.scan_interval_minutes":     "schema_lint",
		"auto_explain.collect_interval_seconds": "auto_explain",
	}
	if owner := exact[path]; owner != "" {
		return owner
	}
	for prefix, owner := range map[string]string{
		"alerting.": "alerting",
		"briefing.": "briefing",
	} {
		if strings.HasPrefix(path, prefix) {
			return owner
		}
	}
	if strings.HasPrefix(path, "llm.") && isLLMResourceField(path) {
		return "llm"
	}
	return ""
}

func isLLMResourceField(path string) bool {
	for _, suffix := range []string{
		"enabled", "endpoint", "api_key", "model",
		"timeout_seconds", "cooldown_seconds",
		"json_mode",
	} {
		if path == "llm."+suffix {
			return true
		}
	}
	return false
}

func changedConfigPaths(current, candidate *Config) []string {
	initializeLifecycleRegistry()
	currentValue := reflect.ValueOf(current).Elem()
	candidateValue := reflect.ValueOf(candidate).Elem()
	changed := make([]string, 0)
	for _, path := range lifecyclePaths {
		entry := lifecycleByPath[path]
		left := currentValue.FieldByIndex(entry.index).Interface()
		right := candidateValue.FieldByIndex(entry.index).Interface()
		if !reflect.DeepEqual(left, right) {
			changed = append(changed, path)
		}
	}
	return changed
}

func configWithPaths(current, candidate *Config, paths []string) *Config {
	initializeLifecycleRegistry()
	combined := Clone(current)
	combinedValue := reflect.ValueOf(combined).Elem()
	candidateValue := reflect.ValueOf(candidate).Elem()
	for _, path := range paths {
		entry, ok := lifecycleByPath[path]
		if !ok {
			continue
		}
		combinedValue.FieldByIndex(entry.index).Set(
			candidateValue.FieldByIndex(entry.index))
	}
	return Clone(combined)
}
