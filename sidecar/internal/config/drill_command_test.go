package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// drill-01c: drill.command.* (restore_argv, destroy_argv, env_allow) are
// YAML-only, restart-bound and checked at load. No concurrent access
// tests: pure functions of the config value.

func absProgram(name string) string {
	return filepath.Join(filepath.VolumeName(mustAbs(".")), string(filepath.Separator),
		"opt", "sage", name)
}

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		panic(err)
	}
	return abs
}

func commandDrillConfig() *Config {
	c := DefaultConfig()
	c.DataDir = mustAbs("sage-data")
	c.Drill.Command = DrillCommandConfig{
		RestoreArgv: []string{absProgram("restore"), "--stanza=main"},
		DestroyArgv: []string{absProgram("destroy")},
		EnvAllow:    []string{"PGBACKREST_REPO1_PATH"}}
	return c
}

func TestDrillCommand_DefaultIsOff(t *testing.T) {
	c := DefaultConfig()
	if c.Drill.Command.Configured() {
		t.Fatalf("default drill.command = %+v, want not configured", c.Drill.Command)
	}
	if err := c.validateDrillCommand(); err != nil {
		t.Fatalf("default config: %v", err)
	}
}

func TestDrillCommand_ValidSelfManagedConfig(t *testing.T) {
	c := commandDrillConfig()
	if err := c.validateDrillCommand(); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	if !c.Drill.Command.Configured() {
		t.Fatal("restore_argv set but not Configured")
	}
	c.Databases = []DatabaseConfig{{Name: "a", Provider: "self-managed"}, {Name: "b"}}
	if err := c.validateDrillCommand(); err != nil {
		t.Fatalf("self-managed fleet refused: %v", err)
	}
}

var drillCommandRefusals = []struct {
	name, want string
	edit       func(*Config)
}{
	{"destroy without restore", "drill.command.restore_argv", func(c *Config) {
		c.Drill.Command.RestoreArgv = nil
	}},
	{"env_allow without restore", "drill.command.restore_argv", func(c *Config) {
		c.Drill.Command.RestoreArgv, c.Drill.Command.DestroyArgv = nil, nil
	}},
	{"empty program", "drill.command.restore_argv[0]", func(c *Config) {
		c.Drill.Command.RestoreArgv = []string{"", "x"}
	}},
	{"relative program", "absolute path", func(c *Config) {
		c.Drill.Command.RestoreArgv = []string{"pgbackrest", "restore"}
	}},
	{"no destroy", "drill.command.destroy_argv", func(c *Config) {
		c.Drill.Command.DestroyArgv = nil
	}},
	{"relative destroy", "drill.command.destroy_argv[0]", func(c *Config) {
		c.Drill.Command.DestroyArgv = []string{"./destroy.sh"}
	}},
	{"nul in an argument", "NUL", func(c *Config) {
		c.Drill.Command.RestoreArgv = []string{absProgram("restore"), "a\x00b"}
	}},
	{"sidecar variable", "SAGE_", func(c *Config) {
		c.Drill.Command.EnvAllow = []string{"sage_database_url"}
	}},
	{"malformed variable", "drill.command.env_allow", func(c *Config) {
		c.Drill.Command.EnvAllow = []string{"A=B"}
	}},
	{"no data_dir", "data_dir", func(c *Config) { c.DataDir = "" }},
	{"managed clone provider", "use the provider's clone kind", func(c *Config) {
		c.Clone.Provider = "rds_restore"
	}},
	{"managed database", "use the provider's clone kind", func(c *Config) {
		c.Databases = []DatabaseConfig{{Name: "orders", Provider: "rds"}}
	}},
}

func TestDrillCommand_Refusals(t *testing.T) {
	for _, tc := range drillCommandRefusals {
		t.Run(tc.name, func(t *testing.T) {
			c := commandDrillConfig()
			tc.edit(c)
			err := c.validateDrillCommand()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestDrillCommand_ProgramLengthBoundary(t *testing.T) {
	c := commandDrillConfig()
	base := absProgram("")
	c.Drill.Command.RestoreArgv = []string{base + strings.Repeat("r", 180-len(base))}
	if err := c.validateDrillCommand(); err != nil {
		t.Fatalf("a 180-character program refused: %v", err)
	}
	c.Drill.Command.RestoreArgv = []string{base + strings.Repeat("r", 181-len(base))}
	if err := c.validateDrillCommand(); err == nil {
		t.Fatal("a 181-character program accepted")
	}
}

func TestDrillCommand_DLEProviderIsSelfManaged(t *testing.T) {
	c := commandDrillConfig()
	c.Clone.Provider, c.Clone.DLEEndpoint = "dle", "http://dle:2345"
	if err := c.validateDrillCommand(); err != nil {
		t.Fatalf("dle (self-managed) with a restore command refused: %v", err)
	}
}

func TestDrillCommand_KeysAreYAMLOnlyAndRestartBound(t *testing.T) {
	for _, key := range []string{"drill.command.restore_argv", "drill.command.destroy_argv",
		"drill.command.env_allow"} {
		// The key registry holds only API-validated keys: a YAML-only key
		// is absent from it.
		if spec, ok := LookupKey(key); ok {
			t.Errorf("%s is API-writable (%+v); it must be YAML-only", key, spec)
		}
		lc, ok := LookupFieldLifecycle(key)
		if !ok || lc.Lifecycle != "restart" {
			t.Errorf("%s lifecycle = %+v, want restart", key, lc)
		}
	}
}

func TestDrillCommand_LoadedFromYAML(t *testing.T) {
	yaml := standaloneIOYAML + `
data_dir: ` + filepath.ToSlash(mustAbs("sage-data")) + `
drill:
  command:
    restore_argv: ["` + filepath.ToSlash(absProgram("restore")) + `", "--stanza=main"]
    destroy_argv: ["` + filepath.ToSlash(absProgram("destroy")) + `"]
    env_allow: [PGBACKREST_REPO1_PATH]
`
	cfg, err := loadYAMLResult(t, yaml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Drill.Command
	if len(got.RestoreArgv) != 2 || got.RestoreArgv[1] != "--stanza=main" ||
		len(got.DestroyArgv) != 1 || len(got.EnvAllow) != 1 ||
		got.EnvAllow[0] != "PGBACKREST_REPO1_PATH" {
		t.Fatalf("drill.command = %+v", got)
	}
	_, err = loadYAMLResult(t, standaloneIOYAML+`
drill:
  command:
    restore_argv: [restore.sh]
`)
	if err == nil || !strings.Contains(err.Error(), "drill.command") {
		t.Fatalf("Load of a relative restore program = %v, want a drill.command error", err)
	}
}
