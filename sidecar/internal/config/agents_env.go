package config

import (
	"fmt"
	"strings"
	"unicode"
)

// DefaultAgentsEnvironment is the environment of a database without a
// verified label (spec §3 principle 6, §5.3).
const DefaultAgentsEnvironment = "prod"

const maxControlDatabaseName = 128

// validateEnvironment checks agents.control_database and
// agents.default_environment. Whether the control database names a
// monitored database is checked when governance starts, since a meta-mode
// fleet is only known at runtime.
func (a AgentsConfig) validateEnvironment() error {
	if a.DefaultEnvironment != DefaultAgentsEnvironment {
		return fmt.Errorf("agents.default_environment must be prod, got %q: an unverified "+
			"binding is always prod; widen one database with "+
			"PUT /api/v1/agent-environments/{database}", a.DefaultEnvironment)
	}
	name := a.ControlDatabase
	if name == "" {
		return nil
	}
	if strings.TrimSpace(name) != name || strings.TrimSpace(name) == "" ||
		len(name) > maxControlDatabaseName ||
		strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("agents.control_database must be a database name of 1-%d "+
			"characters without surrounding spaces or control characters, got %q",
			maxControlDatabaseName, name)
	}
	return nil
}
