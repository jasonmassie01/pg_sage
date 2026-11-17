package config

import "fmt"

// Bounds of clone.restore_deadline_minutes (lab-05d): a full-copy restore
// replays WAL onto a copy of the whole volume, which takes tens of minutes
// to hours.
const (
	CloneRestoreDeadlineMinMinutes = 30
	CloneRestoreDeadlineMaxMinutes = 720
)

// CloneRDSConfig is where RDS rehearsal restores are made (clone.provider
// rds_restore). The source is the instance whose endpoint pg_sage connects
// to; AWS credentials come from the default chain.
type CloneRDSConfig struct {
	Region        string `yaml:"region" doc:"AWS region of the monitored RDS for PostgreSQL instance; its point-in-time restores are made there and the reaper looks for leaked ones there."`
	InstanceClass string `yaml:"instance_class" doc:"Instance class of the restored instance, such as db.m6g.large. Default: the source instance's class."`
}

// validateRDS checks rds_restore: a full copy needs clone.allow_full_copy,
// a region and a restore deadline within bounds. It runs only when
// rds_restore is the provider.
func (c CloneProviderConfig) validateRDS() error {
	if !c.AllowFullCopy {
		return fmt.Errorf("clone.provider rds_restore restores a whole RDS instance for " +
			"each rehearsal (hours, a full instance bill): set clone.allow_full_copy: true " +
			"to opt in, or choose another clone.provider")
	}
	r := c.RDS
	if r.Region == "" {
		return fmt.Errorf("clone.rds.region is required for clone.provider rds_restore")
	}
	if !cloneAuroraRegion.MatchString(r.Region) {
		return fmt.Errorf("clone.rds.region %q must be an AWS region such as us-east-1",
			r.Region)
	}
	if r.InstanceClass != "" && !cloneAuroraClass.MatchString(r.InstanceClass) {
		return fmt.Errorf("clone.rds.instance_class %q must be an RDS instance class such as "+
			"db.m6g.large", r.InstanceClass)
	}
	if c.RestoreDeadlineMinutes < CloneRestoreDeadlineMinMinutes ||
		c.RestoreDeadlineMinutes > CloneRestoreDeadlineMaxMinutes {
		return fmt.Errorf("clone.restore_deadline_minutes must be within %d-%d, got %d",
			CloneRestoreDeadlineMinMinutes, CloneRestoreDeadlineMaxMinutes,
			c.RestoreDeadlineMinutes)
	}
	return nil
}

// fullCopyCostWarning is logged before the first full-copy restore.
func (c CloneProviderConfig) fullCopyCostWarning() string {
	return fmt.Sprintf("clone.provider %s makes full copies: each rehearsal restores a "+
		"whole instance of production's class and storage, billed at the full instance "+
		"price for up to %d minutes of restore plus the rehearsal (the instance-hours are "+
		"on each clone receipt; cost.rates prices them)", c.Provider,
		c.RestoreDeadlineMinutes)
}
