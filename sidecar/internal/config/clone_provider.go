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

// validate checks the clone block. The lakebase, neon and aurora blocks are
// checked only when their provider is chosen, so a leftover block never
// stops another adapter.
func (c CloneProviderConfig) validate() error {
	switch c.Provider {
	case "snapshot":
		return fmt.Errorf("clone.provider snapshot has no adapter in this build: it was "+
			"replaced by the full-copy providers %s; use dle, lakebase_branch, neon_branch, "+
			"aurora_clone, rds_restore or none", strings.Join(CloneFullCopyProviders, ", "))
	case CloneCloudSQLClone, CloneAlloyDBRestore, CloneAzureRestore:
		return fmt.Errorf("clone.provider %s is not in this build yet: use dle, "+
			"lakebase_branch, neon_branch, aurora_clone, rds_restore or none", c.Provider)
	case CloneRDSRestore:
		if err := c.validateRDS(); err != nil {
			return err
		}
	case "none":
	case "dle":
		if c.DLEEndpoint == "" {
			return fmt.Errorf("clone.dle_endpoint is required for dle provider")
		}
	case "lakebase_branch":
		if err := c.Lakebase.validate(); err != nil {
			return err
		}
	case CloneNeonBranch:
		if err := c.Neon.validate(); err != nil {
			return err
		}
	case CloneAuroraClone:
		if err := c.Aurora.validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("clone.provider must be none or dle (Database Lab), " +
			"lakebase_branch (Lakebase), neon_branch (Neon), aurora_clone (Aurora) or " +
			"rds_restore (RDS)")
	}
	if c.MaxCloneAgeMinutes <= 0 {
		return fmt.Errorf("clone.max_clone_age_minutes must be positive")
	}
	return c.validateRehearsals()
}

// The full-copy clone providers (lab-05a): each restores a whole instance,
// so each needs clone.allow_full_copy. They replace clone.provider
// snapshot; lab-05d built rds_restore, lab-05e to lab-05g add the rest.
const (
	CloneRDSRestore     = "rds_restore"
	CloneCloudSQLClone  = "cloudsql_clone"
	CloneAlloyDBRestore = "alloydb_restore"
	CloneAzureRestore   = "azure_restore"
)

// CloneFullCopyProviders are the full-copy clone.provider names.
var CloneFullCopyProviders = []string{CloneRDSRestore, CloneCloudSQLClone,
	CloneAlloyDBRestore, CloneAzureRestore}

// CloneMaxCreatesPerHour bounds clone.max_creates_per_hour.
const CloneMaxCreatesPerHour = 60

// Warnings are the clone settings that are valid but do nothing
// (neon_branch without an API key, allow_full_copy without a full-copy
// provider) and the cost of a full-copy provider, logged at start before
// any restore.
func (c CloneProviderConfig) Warnings() []string {
	if c.Provider == CloneNeonBranch && strings.TrimSpace(c.Neon.APIKey) == "" {
		return []string{"clone.provider neon_branch has no clone.neon.api_key (or " +
			"SAGE_CLONE_NEON_API_KEY): no branch can be made, rehearsals are recommend-only"}
	}
	if !c.AllowFullCopy {
		return nil
	}
	for _, name := range CloneFullCopyProviders {
		if c.Provider == name {
			return []string{c.fullCopyCostWarning()}
		}
	}
	provider := c.Provider
	if provider == "" {
		provider = "none"
	}
	return []string{fmt.Sprintf("clone.allow_full_copy is true but clone.provider %s "+
		"makes no full copies: the setting has no effect", provider)}
}

// Bounds of the index rehearsal settings (lab-04a).
const (
	CloneMaxRehearsalsPerHour               = 60
	CloneMaxRehearsalStatementTimeoutSecond = 300
)

// validateRehearsals checks the index rehearsal settings; 0 is unset and
// the default applies.
func (c CloneProviderConfig) validateRehearsals() error {
	if c.MaxCreatesPerHour < 0 || c.MaxCreatesPerHour > CloneMaxCreatesPerHour {
		return fmt.Errorf("clone.max_creates_per_hour must be within 1-%d, got %d",
			CloneMaxCreatesPerHour, c.MaxCreatesPerHour)
	}
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
