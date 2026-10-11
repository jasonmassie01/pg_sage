package config

import (
	"fmt"
	"regexp"
	"strings"
)

// The kill switch's settings (spec §6.10, §9):
// agents.kill_verify_timeout_seconds bounds the verification that no agent
// backend remains, and databases[].replicas lists the standbys the kill
// reaches (it reports the others it sees in pg_stat_replication).

// DatabaseReplica is one configured replica of a monitored database. The
// DSN is read from an environment variable, never from the file.
type DatabaseReplica struct {
	Name   string `yaml:"name" doc:"Name of the replica in kill reports; unique within its database. Match it to the standby's cluster_name so the kill does not also report it as unconfigured."`
	DSNEnv string `yaml:"dsn_env" doc:"Environment variable holding the replica's connection string. The agent kill switch connects with it to end agent sessions on the replica."`
}

// DefaultKillVerifyTimeoutSeconds is the spec's 10 s kill bound.
const DefaultKillVerifyTimeoutSeconds = 10

// DefaultKillFallbackLog is the local audit of kills that ran without the
// gate or the control database, relative to the working directory.
const DefaultKillFallbackLog = "agent-kill-fallback.log"

// maxKillVerifyTimeoutSeconds caps the verification at an hour.
const maxKillVerifyTimeoutSeconds = 3600

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// validateKill checks agents.kill_verify_timeout_seconds.
func (a AgentsConfig) validateKill() error {
	s := a.KillVerifyTimeoutSeconds
	if s < 1 || s > maxKillVerifyTimeoutSeconds {
		return fmt.Errorf("agents.kill_verify_timeout_seconds must be 1 to %d, got %d",
			maxKillVerifyTimeoutSeconds, s)
	}
	p := a.KillFallbackLog
	if p == "" || len(p) > 4096 || strings.ContainsAny(p, "\x00\n\r") {
		return fmt.Errorf("agents.kill_fallback_log must be a file path")
	}
	return nil
}

// validateReplicas checks every databases[].replicas entry.
func validateReplicas(dbs []DatabaseConfig) error {
	for i, db := range dbs {
		seen := make(map[string]bool, len(db.Replicas))
		for j, r := range db.Replicas {
			at := fmt.Sprintf("databases[%d].replicas[%d]", i, j)
			switch {
			case r.Name == "" || len(r.Name) > 200:
				return fmt.Errorf("%s: name must be 1 to 200 characters", at)
			case seen[r.Name]:
				return fmt.Errorf("%s: duplicate replica name %q", at, r.Name)
			case r.DSNEnv == "":
				return fmt.Errorf("%s: dsn_env must name the environment variable holding "+
					"the replica's DSN", at)
			case !envNamePattern.MatchString(r.DSNEnv):
				return fmt.Errorf("%s: dsn_env must be an environment variable name "+
					"(letters, digits, underscore), not a DSN", at)
			}
			seen[r.Name] = true
		}
	}
	return nil
}
