package config

import (
	"flag"
	"os"
	"reflect"

	"gopkg.in/yaml.v3"
)

// config-03: Load records which dotted keys the file layer set explicitly:
// the YAML file, a SAGE_* variable (or its _FILE secret), or a flag. It is
// a presence set, not per-field source tracking: an explicit value equal to
// its default (trust.level: observation) is still explicit, so a persisted
// override that replaces it is reported as masked.

// envExplicitPaths maps each environment variable that sets a config field
// to that field's dotted key.
var envExplicitPaths = map[string]string{
	"SAGE_MODE":                                "mode",
	"SAGE_DATABASE_URL":                        "postgres.database_url",
	"SAGE_PG_HOST":                             "postgres.host",
	"SAGE_PG_PORT":                             "postgres.port",
	"SAGE_PG_USER":                             "postgres.user",
	"SAGE_PG_PASSWORD":                         "postgres.password",
	"SAGE_PG_DATABASE":                         "postgres.database",
	"SAGE_PG_SSLMODE":                          "postgres.sslmode",
	"SAGE_PG_MAX_CONNS":                        "postgres.max_connections",
	"SAGE_PROMETHEUS_PORT":                     "prometheus.listen_addr",
	"SAGE_TRUST_LEVEL":                         "trust.level",
	"SAGE_LLM_API_KEY":                         "llm.api_key",
	"SAGE_LLM_AUTH":                            "llm.auth",
	"SAGE_LLM_ENDPOINT":                        "llm.endpoint",
	"SAGE_LLM_MODEL":                           "llm.model",
	"SAGE_OPTIMIZER_LLM_API_KEY":               "llm.optimizer_llm.api_key",
	"SAGE_OPTIMIZER_LLM_ENDPOINT":              "llm.optimizer_llm.endpoint",
	"SAGE_OPTIMIZER_LLM_MODEL":                 "llm.optimizer_llm.model",
	"SAGE_POLICY_PROFILE":                      "policy.profile",
	"SAGE_VALUE_TOIL_MODEL_VERSION":            "value.toil_model_version",
	"SAGE_VERIFY_WINDOW_MINUTES":               "verify.window_minutes",
	"SAGE_VERIFY_WINDOW_MAX_MINUTES":           "verify.window_max_minutes",
	"SAGE_VERIFY_MIN_GAIN_PCT":                 "verify.min_gain_pct",
	"SAGE_VERIFY_REGRESS_PCT":                  "verify.regress_pct",
	"SAGE_VERIFY_WRITE_IMPACT_PCT":             "verify.write_impact_pct",
	"SAGE_VERIFY_MIN_SAMPLES":                  "verify.min_samples",
	"SAGE_CLONE_PROVIDER":                      "clone.provider",
	"SAGE_CLONE_DLE_ENDPOINT":                  "clone.dle_endpoint",
	"SAGE_CLONE_DLE_TOKEN":                     "clone.dle_token",
	"SAGE_CLONE_NEON_API_KEY":                  "clone.neon.api_key",
	"SAGE_CLONE_MAX_AGE_MINUTES":               "clone.max_clone_age_minutes",
	"SAGE_CUSTODIAN_FREEZE_RED_BUFFER_PCT":     "custodian.freeze.red_buffer_pct",
	"SAGE_CUSTODIAN_WAL_ABANDON_AFTER_MINUTES": "custodian.wal.abandon_after_minutes",
	"SAGE_CUSTODIAN_WAL_DISK_PCT_CEILING":      "custodian.wal.retained_wal_disk_pct_ceiling",
	"SAGE_MCP_ENABLED":                         "mcp.enabled",
	"SAGE_MCP_TRANSPORT":                       "mcp.transport",
}

// flagExplicitPaths maps each Load flag that sets a config field to its key.
var flagExplicitPaths = map[string]string{
	"mode":        "mode",
	"pg-host":     "postgres.host",
	"pg-port":     "postgres.port",
	"pg-user":     "postgres.user",
	"pg-password": "postgres.password",
	"pg-database": "postgres.database",
	"pg-sslmode":  "postgres.sslmode",
	"pg-url":      "postgres.database_url",
	"prom-addr":   "prometheus.listen_addr",
}

// IsExplicit reports whether the file layer set path at Load. A config that
// was never loaded (DefaultConfig, a hand-built one) has no explicit path.
func (c *Config) IsExplicit(path string) bool {
	if c == nil {
		return false
	}
	_, ok := c.explicitPaths[path]
	return ok
}

func (c *Config) markExplicit(path string) {
	if c.explicitPaths == nil {
		c.explicitPaths = make(map[string]struct{})
	}
	c.explicitPaths[path] = struct{}{}
}

// markEnvPaths records the variables the overlay read: a non-empty value,
// or a NAME_FILE secret file.
func markEnvPaths(cfg *Config) {
	for name, path := range envExplicitPaths {
		if os.Getenv(name) != "" || os.Getenv(name+"_FILE") != "" {
			cfg.markExplicit(path)
		}
	}
}

// markFlagPaths records the flags given with a non-empty value; Load
// ignores an empty one.
func markFlagPaths(cfg *Config, fs *flag.FlagSet) {
	fs.Visit(func(f *flag.Flag) {
		path, ok := flagExplicitPaths[f.Name]
		if ok && f.Value.String() != "" && f.Value.String() != "0" {
			cfg.markExplicit(path)
		}
	})
}

// yamlPresencePaths walks the decoded document and returns the dotted key
// of every leaf it sets. A null leaf sets nothing, so it is absent.
func yamlPresencePaths(raw string) (map[string]struct{}, error) {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		return nil, err
	}
	paths := make(map[string]struct{})
	if len(document.Content) > 0 {
		collectYAMLPaths(document.Content[0], "", paths)
	}
	return paths, nil
}

func collectYAMLPaths(node *yaml.Node, prefix string, paths map[string]struct{}) {
	if node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		path := node.Content[i].Value
		if prefix != "" {
			path = prefix + "." + path
		}
		value := node.Content[i+1]
		switch {
		case value.Kind == yaml.MappingNode:
			collectYAMLPaths(value, path, paths)
		case value.Kind == yaml.ScalarNode && value.Tag == "!!null":
		default:
			paths[path] = struct{}{}
		}
	}
}

// ValueAtPath returns the field at a dotted config key, or false when the
// key is not a config leaf.
func ValueAtPath(cfg *Config, path string) (any, bool) {
	if cfg == nil {
		return nil, false
	}
	initializeLifecycleRegistry()
	entry, ok := lifecycleByPath[path]
	if !ok {
		return nil, false
	}
	return reflect.ValueOf(cfg).Elem().FieldByIndex(entry.index).Interface(), true
}
