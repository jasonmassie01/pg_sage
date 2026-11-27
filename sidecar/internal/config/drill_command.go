package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// DrillCommandConfig is a self-managed restore command for restore drills
// (drill-01c): an operator program restores the database's own backups
// (pgBackRest, Barman, WAL-G) and prints where. It runs on the sidecar
// host, so the keys are YAML-only and restart-bound, never API-writable.
type DrillCommandConfig struct {
	RestoreArgv []string `yaml:"restore_argv" doc:"Self-managed restore drill program and arguments (absolute path, no shell); prints one JSON line {dsn, id, recovery_point}. Needs clone.allow_full_copy. YAML only, restart-bound."`
	DestroyArgv []string `yaml:"destroy_argv" doc:"Required with drill.command.restore_argv: program and arguments that remove a restore, given SAGE_CLONE_ID; may run twice for one id. YAML only, restart-bound."`
	EnvAllow    []string `yaml:"env_allow" doc:"Sidecar environment variable names passed to the drill.command programs besides PATH. SAGE_ names are refused; values are never logged. YAML only, restart-bound."`
}

// Configured reports whether a restore command is set.
func (d DrillCommandConfig) Configured() bool { return len(d.RestoreArgv) > 0 }

// maxDrillCommandProgram bounds a program path, which names the drill
// receipts' scope.
const maxDrillCommandProgram = 180

var drillCommandEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateDrillCommand checks drill.command against the rest of the
// config: an absolute program for both commands, a data_dir to run in,
// and self-managed databases only.
func (c *Config) validateDrillCommand() error {
	d := c.Drill.Command
	if !d.Configured() {
		if len(d.DestroyArgv) > 0 || len(d.EnvAllow) > 0 {
			return fmt.Errorf("drill.command.destroy_argv and env_allow need " +
				"drill.command.restore_argv")
		}
		return nil
	}
	for key, argv := range map[string][]string{"restore_argv": d.RestoreArgv,
		"destroy_argv": d.DestroyArgv} {
		if err := validateDrillArgv("drill.command."+key, argv); err != nil {
			return err
		}
	}
	for _, name := range d.EnvAllow {
		if !drillCommandEnvName.MatchString(name) {
			return fmt.Errorf("drill.command.env_allow %q is not a variable name", name)
		}
		if strings.HasPrefix(strings.ToUpper(name), "SAGE_") {
			return fmt.Errorf("drill.command.env_allow %q: SAGE_ variables hold the "+
				"sidecar's own settings and secrets and are never passed", name)
		}
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return fmt.Errorf("drill.command.restore_argv needs data_dir (or SAGE_DATA_DIR): " +
			"the programs run in data_dir/restore-drills")
	}
	return c.refuseManagedDrillCommand()
}

func validateDrillArgv(key string, argv []string) error {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return fmt.Errorf("%s[0] must name the program", key)
	}
	if !filepath.IsAbs(argv[0]) {
		return fmt.Errorf("%s[0] %q must be an absolute path (no PATH lookup, no shell)",
			key, argv[0])
	}
	if len(argv[0]) > maxDrillCommandProgram {
		return fmt.Errorf("%s[0] must be at most %d characters", key, maxDrillCommandProgram)
	}
	for i, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("%s[%d] holds a NUL character", key, i)
		}
	}
	return nil
}

// refuseManagedDrillCommand refuses a restore command next to a managed
// clone provider or a database declared managed: there the provider's own
// clone kind proves its backups.
func (c *Config) refuseManagedDrillCommand() error {
	switch c.Clone.Provider {
	case "", "none", "dle":
	default:
		return fmt.Errorf("drill.command.restore_argv is for self-managed databases, but "+
			"clone.provider is %s: use the provider's clone kind", c.Clone.Provider)
	}
	providers := map[string]string{"postgres": c.Postgres.Provider}
	for _, db := range c.Databases {
		providers["databases["+db.Name+"]"] = db.Provider
	}
	for where, p := range providers {
		if p != "" && p != "self-managed" {
			return fmt.Errorf("drill.command.restore_argv is for self-managed databases, "+
				"but %s.provider is %s: use the provider's clone kind", where, p)
		}
	}
	return nil
}
