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
)

// Reconfiguration owners production registers. Field metadata names no
// other owner; see ProductionOwners.
const (
	TrustPolicyOwner = "trust_policy"
	LLMOwner         = "llm"
)

// ProductionOwners returns every reconfiguration owner the sidecar registers
// in some mode: trust_policy, llm, llm_redaction, briefing and alerting in
// every mode,
// fleet_databases in YAML fleet mode. The result is a fresh copy.
func ProductionOwners() []string {
	return []string{TrustPolicyOwner, LLMOwner, LLMRedactionOwner, BriefingOwner,
		AlertingOwner, FleetDatabasesOwner}
}

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
	// The YAML fleet's databases (with the defaults folded into them) are
	// reconciled per database; see DatabaseFieldLifecycles.
	if path == "databases" || strings.HasPrefix(path, "defaults.") {
		return LifecycleReconfigure, FleetDatabasesOwner
	}
	if isRestartField(path) {
		return LifecycleRestart, ""
	}
	if owner := reconfigurationOwner(path); owner != "" {
		return LifecycleReconfigure, owner
	}
	if path == "trust.level" || liveTrustCeilingFields[path] {
		return LifecycleLivePolicy, TrustPolicyOwner
	}
	// A field is restart-bound until a concrete runtime consumer proves it can
	// adopt an immutable snapshot or supplies a reconfiguration owner.
	return LifecycleRestart, ""
}

func isRestartField(path string) bool {
	if path == "mode" || path == "meta_db" || path == "encryption_key" ||
		path == "encryption_key_previous" || path == "data_dir" {
		return true
	}
	if strings.HasPrefix(path, "postgres.") || strings.HasPrefix(path, "oauth.") {
		return true
	}
	return path == "api.listen_addr" || path == "api.trusted_proxies" ||
		path == "api.trusted_origins" || path == "prometheus.listen_addr" ||
		path == "prometheus.serve_on_api"
}

// reconfigurationOwner names the production owner that rebuilds a field in
// place: the LLM connection, the LLM redaction switch and the briefing
// schedule and channels. A field whose owner production never registers
// would be labelled reconfigure yet wait for a restart in every mode
// (config-07), so it stays restart-bound until an owner exists.
func reconfigurationOwner(path string) string {
	if path == "llm.redact_numeric_literals" {
		return LLMRedactionOwner // llm-09: process-wide, registered in every mode
	}
	switch path {
	case "briefing.schedule", "briefing.channels", "briefing.slack_webhook_url":
		return BriefingOwner // llm-06: briefing.fleet_mode stays restart-bound
	case "alerting.cooldown_minutes", "alerting.quiet_hours_start",
		"alerting.quiet_hours_end", "alerting.timezone", "alerting.repeat_minutes":
		return AlertingOwner // alerting-06
	case "alerting.enabled", "alerting.routes", "alerting.webhooks",
		"alerting.slack_webhook_url", "alerting.pagerduty_routing_key":
		// alerting-06b: reconciled live into the notification tables;
		// alerting.engine and check_interval_seconds stay restart-bound.
		return AlertingOwner
	}
	if strings.HasPrefix(path, "llm.") && isLLMResourceField(path) {
		return LLMOwner
	}
	return ""
}

func isLLMResourceField(path string) bool {
	for _, suffix := range []string{
		"enabled", "endpoint", "api_key", "model", "auth",
		"timeout_seconds", "cooldown_seconds",
		"json_mode", "token_parameter", "tool_reasoning_effort",
		"global_token_budget_daily",
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
