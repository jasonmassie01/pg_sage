package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestLoadYAMLRejectsUnknownKeysWithoutMutatingCandidate(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		path string
	}{
		{"top level", "unknown_agent_feature:\n  enabled: true\n", "unknown_agent_feature"},
		{"nested", "collector:\n  interval_second: 12\n", "interval_second"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			before := Clone(cfg)
			err := loadYAML(writeYAMLFile(t, tt.yaml), cfg)
			if err == nil || !strings.Contains(err.Error(), tt.path) {
				t.Fatalf("loadYAML error = %v, want unknown path %q", err, tt.path)
			}
			if !reflect.DeepEqual(cfg, before) {
				t.Fatal("rejected YAML partially mutated the candidate")
			}
		})
	}
}

func TestLoadYAMLStrictStillPreservesExplicitFalseAndEmptyCollections(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LLM.Enabled = true
	cfg.Alerting.Routes = []AlertRoute{{Severity: "critical", Channels: []string{"slack"}}}
	yaml := "llm:\n  enabled: false\nalerting:\n  routes: []\n"

	if err := loadYAML(writeYAMLFile(t, yaml), cfg); err != nil {
		t.Fatalf("loadYAML: %v", err)
	}
	if cfg.LLM.Enabled {
		t.Fatal("explicit false was treated as absent")
	}
	if cfg.Alerting.Routes == nil || len(cfg.Alerting.Routes) != 0 {
		t.Fatalf("explicit empty routes = %#v, want non-nil empty slice", cfg.Alerting.Routes)
	}
}

func writeYAMLFile(t *testing.T, data string) string {
	t.Helper()
	path := t.TempDir() + string(os.PathSeparator) + "config.yaml"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
