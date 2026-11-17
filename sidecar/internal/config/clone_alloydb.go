package config

import (
	"fmt"
	"regexp"
)

var (
	cloneGCPRegion = regexp.MustCompile(`^[a-z]+(-[a-z]+)+[0-9]+$`)
	cloneAlloyDBID = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

const alloyDBRequired = "is required for clone.provider alloydb_restore"

// CloneAlloyDBConfig is the AlloyDB cluster rehearsal clones are restored
// from (clone.provider alloydb_restore, lab-05f), in the same project and
// region. Google credentials come from the Application Default
// Credentials chain.
type CloneAlloyDBConfig struct {
	Project  string `yaml:"project" doc:"Google Cloud project of the monitored AlloyDB cluster; restores are made there and the reaper looks for leaked ones there."`
	Location string `yaml:"location" doc:"Region of the monitored AlloyDB cluster, such as us-central1; restores are made in the same region."`
	Cluster  string `yaml:"cluster" doc:"Cluster id of the monitored AlloyDB cluster; it needs continuous backup on, and pg_sage must connect to its primary instance's IP."`
}

// validateAlloyDB checks alloydb_restore: a full copy needs
// clone.allow_full_copy, a project, a region, a cluster id and a restore
// deadline within bounds. It runs only when alloydb_restore is the
// provider.
func (c CloneProviderConfig) validateAlloyDB() error {
	if !c.AllowFullCopy {
		return fmt.Errorf("clone.provider alloydb_restore restores a whole AlloyDB cluster " +
			"for each rehearsal (a full cluster bill while it lives): set " +
			"clone.allow_full_copy: true to opt in, or choose another clone.provider")
	}
	a := c.AlloyDB
	switch {
	case a.Project == "":
		return fmt.Errorf("clone.alloydb.project %s", alloyDBRequired)
	case !cloneGCPProject.MatchString(a.Project):
		return fmt.Errorf("clone.alloydb.project %q must be a Google Cloud project id such "+
			"as acme-prod-1234", a.Project)
	case a.Location == "":
		return fmt.Errorf("clone.alloydb.location %s", alloyDBRequired)
	case !cloneGCPRegion.MatchString(a.Location):
		return fmt.Errorf("clone.alloydb.location %q must be a Google Cloud region such as "+
			"us-central1 (not a zone)", a.Location)
	case a.Cluster == "":
		return fmt.Errorf("clone.alloydb.cluster %s", alloyDBRequired)
	case !cloneAlloyDBID.MatchString(a.Cluster) || clonePgSageName.MatchString(a.Cluster):
		return fmt.Errorf("clone.alloydb.cluster %q must be an AlloyDB cluster id (1-63 "+
			"lowercase letters, digits and hyphens) and not a pg_sage clone", a.Cluster)
	}
	return c.validateRestoreDeadline()
}
