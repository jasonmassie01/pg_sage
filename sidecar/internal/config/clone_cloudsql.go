package config

import (
	"fmt"
	"regexp"
)

var (
	cloneGCPProject = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	cloneSQLName    = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,95}[a-z0-9])?$`)
	// clonePgSageName is a pg_sage clone's name: never a clone source.
	clonePgSageName = regexp.MustCompile(`^sage-[a-z]+(-[a-z]+)*-[0-9a-f]{8}$`)
)

// CloneCloudSQLConfig is the Cloud SQL instance rehearsal clones are made
// from (clone.provider cloudsql_clone, lab-05e), in the same project.
// Google credentials come from the Application Default Credentials chain.
type CloneCloudSQLConfig struct {
	Project  string `yaml:"project" doc:"Google Cloud project of the monitored Cloud SQL for PostgreSQL instance; clones are made there and the reaper looks for leaked ones there."`
	Instance string `yaml:"instance" doc:"Name of the monitored Cloud SQL for PostgreSQL instance (its instance id, not the connection name); pg_sage must connect to one of its IPs."`
}

// validateCloudSQL checks cloudsql_clone: a full copy needs
// clone.allow_full_copy, a project, an instance and a restore deadline
// within bounds. It runs only when cloudsql_clone is the provider.
func (c CloneProviderConfig) validateCloudSQL() error {
	if !c.AllowFullCopy {
		return fmt.Errorf("clone.provider cloudsql_clone clones a whole Cloud SQL instance " +
			"for each rehearsal (a full instance bill while it lives): set " +
			"clone.allow_full_copy: true to opt in, or choose another clone.provider")
	}
	s := c.CloudSQL
	if s.Project == "" {
		return fmt.Errorf("clone.cloudsql.project is required for clone.provider " +
			"cloudsql_clone")
	}
	if !cloneGCPProject.MatchString(s.Project) {
		return fmt.Errorf("clone.cloudsql.project %q must be a Google Cloud project id such "+
			"as acme-prod-1234", s.Project)
	}
	if s.Instance == "" {
		return fmt.Errorf("clone.cloudsql.instance is required for clone.provider " +
			"cloudsql_clone")
	}
	if !cloneSQLName.MatchString(s.Instance) || clonePgSageName.MatchString(s.Instance) {
		return fmt.Errorf("clone.cloudsql.instance %q must be a Cloud SQL instance name "+
			"(lowercase letters, digits and hyphens) and not a pg_sage clone", s.Instance)
	}
	return c.validateRestoreDeadline()
}

// validateRestoreDeadline bounds clone.restore_deadline_minutes for a
// full-copy provider.
func (c CloneProviderConfig) validateRestoreDeadline() error {
	if c.RestoreDeadlineMinutes < CloneRestoreDeadlineMinMinutes ||
		c.RestoreDeadlineMinutes > CloneRestoreDeadlineMaxMinutes {
		return fmt.Errorf("clone.restore_deadline_minutes must be within %d-%d, got %d",
			CloneRestoreDeadlineMinMinutes, CloneRestoreDeadlineMaxMinutes,
			c.RestoreDeadlineMinutes)
	}
	return nil
}
