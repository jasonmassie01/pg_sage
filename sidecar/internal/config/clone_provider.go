package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/lakebaseid"
)

// CloneLakebaseMaxTTLMinutes bounds clone.lakebase.ttl_minutes: a leaked
// rehearsal branch holds production data for at most a day.
const CloneLakebaseMaxTTLMinutes = 1440

var cloneLakebaseID = regexp.MustCompile(`^` + lakebaseid.Pattern + `$`)

// validate checks the clone block. The lakebase block is checked only when
// lakebase_branch is the provider, so a leftover block never stops another
// adapter.
func (c CloneProviderConfig) validate() error {
	switch c.Provider {
	case "snapshot":
		return fmt.Errorf("clone.provider snapshot has no adapter in this build: " +
			"use dle, lakebase_branch or none")
	case "none":
	case "dle":
		if c.DLEEndpoint == "" {
			return fmt.Errorf("clone.dle_endpoint is required for dle provider")
		}
	case "lakebase_branch":
		if err := c.Lakebase.validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("clone.provider must be none or dle (Database Lab) or " +
			"lakebase_branch (Lakebase)")
	}
	if c.MaxCloneAgeMinutes <= 0 {
		return fmt.Errorf("clone.max_clone_age_minutes must be positive")
	}
	return c.validateRehearsals()
}

// Bounds of the index rehearsal settings (lab-04a).
const (
	CloneMaxRehearsalsPerHour               = 60
	CloneMaxRehearsalStatementTimeoutSecond = 300
)

// validateRehearsals checks the index rehearsal settings; 0 is unset and
// the default applies.
func (c CloneProviderConfig) validateRehearsals() error {
	if c.RehearsalsPerHour < 0 || c.RehearsalsPerHour > CloneMaxRehearsalsPerHour {
		return fmt.Errorf("clone.rehearsals_per_hour must be within 1-%d, got %d",
			CloneMaxRehearsalsPerHour, c.RehearsalsPerHour)
	}
	if c.RehearsalStatementTimeoutSeconds < 0 ||
		c.RehearsalStatementTimeoutSeconds > CloneMaxRehearsalStatementTimeoutSecond {
		return fmt.Errorf("clone.rehearsal_statement_timeout_seconds must be within 1-%d, "+
			"got %d", CloneMaxRehearsalStatementTimeoutSecond,
			c.RehearsalStatementTimeoutSeconds)
	}
	return nil
}

func (l CloneLakebaseConfig) validate() error {
	if l.Project == "" {
		return fmt.Errorf("clone.lakebase.project is required for clone.provider " +
			"lakebase_branch")
	}
	if !cloneLakebaseID.MatchString(l.Project) {
		return fmt.Errorf("clone.lakebase.project %q must be a project id: 1-63 lowercase "+
			"letters, digits or hyphens", l.Project)
	}
	if err := l.validateSource(); err != nil {
		return err
	}
	if l.TTLMinutes < 1 || l.TTLMinutes > CloneLakebaseMaxTTLMinutes {
		return fmt.Errorf("clone.lakebase.ttl_minutes must be within 1-%d, got %d",
			CloneLakebaseMaxTTLMinutes, l.TTLMinutes)
	}
	if !(l.EndpointMaxCU > 0) {
		return fmt.Errorf("clone.lakebase.endpoint_max_cu must be positive, got %v",
			l.EndpointMaxCU)
	}
	return nil
}

// validateSource accepts "" (the monitored endpoint's branch), a branch id,
// or projects/<project>/branches/<id> of the configured project.
func (l CloneLakebaseConfig) validateSource() error {
	id := l.SourceBranch
	if id == "" {
		return nil
	}
	if strings.HasPrefix(id, "projects/") {
		prefix := "projects/" + l.Project + "/branches/"
		if !strings.HasPrefix(id, prefix) {
			return fmt.Errorf("clone.lakebase.source_branch %q is not in project %q",
				l.SourceBranch, l.Project)
		}
		id = strings.TrimPrefix(id, prefix)
	}
	if !cloneLakebaseID.MatchString(id) {
		return fmt.Errorf("clone.lakebase.source_branch %q must be a branch id (1-63 "+
			"lowercase letters, digits or hyphens) or projects/%s/branches/<id>",
			l.SourceBranch, l.Project)
	}
	return nil
}
