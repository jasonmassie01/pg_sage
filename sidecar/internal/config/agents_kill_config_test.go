package config

import (
	"strings"
	"testing"
)

// agents.kill_verify_timeout_seconds and databases[].replicas (spec §9,
// §6.10): the kill verifies within the timeout and reaches only the
// replicas listed per database, by a DSN read from an environment
// variable (never a DSN in the file).

func TestAgentsKillDefaults(t *testing.T) {
	c := DefaultConfig()
	if c.Agents.KillVerifyTimeoutSeconds != 10 {
		t.Fatalf("kill_verify_timeout_seconds = %d, want 10",
			c.Agents.KillVerifyTimeoutSeconds)
	}
	if c.Agents.KillFallbackLog != "agent-kill-fallback.log" {
		t.Fatalf("kill_fallback_log = %q", c.Agents.KillFallbackLog)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
}

// Default value masking: an agents block without the key keeps 10.
func TestAgentsKillPartialYAMLKeepsDefault(t *testing.T) {
	path := wave5WriteYAML(t, "agents:\n  single_operator_mode: true\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if c.Agents.KillVerifyTimeoutSeconds != 10 {
		t.Fatalf("kill_verify_timeout_seconds = %d", c.Agents.KillVerifyTimeoutSeconds)
	}
}

func TestAgentsKillReplicasLoad(t *testing.T) {
	path := wave5WriteYAML(t, "mode: fleet\nagents:\n  kill_verify_timeout_seconds: 4\n"+
		"databases:\n  - name: orders\n    host: db1\n    replicas:\n"+
		"      - name: replica1\n        dsn_env: ORDERS_REPLICA1_DSN\n"+
		"      - name: replica2\n        dsn_env: ORDERS_REPLICA2_DSN\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if c.Agents.KillVerifyTimeoutSeconds != 4 {
		t.Fatalf("timeout = %d", c.Agents.KillVerifyTimeoutSeconds)
	}
	got := c.Databases[0].Replicas
	if len(got) != 2 || got[0] != (DatabaseReplica{Name: "replica1",
		DSNEnv: "ORDERS_REPLICA1_DSN"}) || got[1].Name != "replica2" {
		t.Fatalf("replicas = %+v", got)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	clone := Clone(c)
	clone.Databases[0].Replicas[0].Name = "changed"
	if c.Databases[0].Replicas[0].Name != "replica1" {
		t.Fatal("Clone shares the replicas slice")
	}
}

func TestAgentsKillValidation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"zero timeout": {func(c *Config) { c.Agents.KillVerifyTimeoutSeconds = 0 },
			"agents.kill_verify_timeout_seconds"},
		"negative timeout": {func(c *Config) { c.Agents.KillVerifyTimeoutSeconds = -1 },
			"agents.kill_verify_timeout_seconds"},
		"huge timeout": {func(c *Config) { c.Agents.KillVerifyTimeoutSeconds = 3601 },
			"agents.kill_verify_timeout_seconds"},
		"empty fallback log": {func(c *Config) { c.Agents.KillFallbackLog = "" },
			"agents.kill_fallback_log"},
		"control char log": {func(c *Config) { c.Agents.KillFallbackLog = "a\nb" },
			"agents.kill_fallback_log"},
		"replica no name": {func(c *Config) {
			c.Databases = []DatabaseConfig{{Name: "a", Host: "h",
				Replicas: []DatabaseReplica{{DSNEnv: "A_DSN"}}}}
		}, "databases[0].replicas[0]: name"},
		"replica no env": {func(c *Config) {
			c.Databases = []DatabaseConfig{{Name: "a", Host: "h",
				Replicas: []DatabaseReplica{{Name: "r"}}}}
		}, "dsn_env"},
		"replica dsn in file": {func(c *Config) {
			c.Databases = []DatabaseConfig{{Name: "a", Host: "h",
				Replicas: []DatabaseReplica{{Name: "r", DSNEnv: "postgres://u:p@h/db"}}}}
		}, "environment variable name"},
		"replica duplicate": {func(c *Config) {
			c.Databases = []DatabaseConfig{{Name: "a", Host: "h",
				Replicas: []DatabaseReplica{{Name: "r", DSNEnv: "A"}, {Name: "r",
					DSNEnv: "B"}}}}
		}, "duplicate replica"},
	}
	for name, tc := range cases {
		c := DefaultConfig()
		tc.mutate(c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
	ok := DefaultConfig()
	ok.Agents.KillVerifyTimeoutSeconds = 1
	if err := ok.Validate(); err != nil {
		t.Fatalf("the 1 s boundary was rejected: %v", err)
	}
	ok.Agents.KillVerifyTimeoutSeconds = 3600
	if err := ok.Validate(); err != nil {
		t.Fatalf("the 3600 s boundary was rejected: %v", err)
	}
}
