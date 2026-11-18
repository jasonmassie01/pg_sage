package config

import (
	"fmt"
	"regexp"
)

// CloneAuroraClone is the clone.provider of Aurora copy-on-write clone
// rehearsals (lab-05c).
const CloneAuroraClone = "aurora_clone"

// Bounds of clone.aurora.ttl_minutes: a clone takes 10 to 20 minutes to
// be ready, and a leaked one holds production data for at most a day.
const (
	CloneAuroraMinTTLMinutes = 30
	CloneAuroraMaxTTLMinutes = 1440
)

var (
	cloneAuroraRegion = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-[0-9]$`)
	cloneAuroraClass  = regexp.MustCompile(`^db\.[a-z0-9]+(\.[a-z0-9]+)*$`)
)

// CloneAuroraConfig is where Aurora rehearsal clones are made
// (clone.provider aurora_clone). The source is the cluster whose endpoint
// pg_sage connects to; AWS credentials come from the default chain.
type CloneAuroraConfig struct {
	Region        string `yaml:"region" doc:"AWS region of the monitored Aurora PostgreSQL cluster; its copy-on-write clones are made there and the reaper looks for leaked ones there."`
	InstanceClass string `yaml:"instance_class" doc:"Instance class of the clone's one instance, such as db.r6g.large or db.serverless. Default: the source writer's class (db.serverless for a Serverless v2 writer)."`
	TTLMinutes    int    `yaml:"ttl_minutes" doc:"Longest a clone lives, in minutes, 30 to 1440; tagged on the clone, and a rehearsal ends by then. Aurora has no server-side expiry, so the reaper destroys a leaked clone. Default: 60."`
}

// validate checks the block; it runs only when aurora_clone is the
// provider.
func (a CloneAuroraConfig) validate() error {
	if a.Region == "" {
		return fmt.Errorf("clone.aurora.region is required for clone.provider aurora_clone")
	}
	if !cloneAuroraRegion.MatchString(a.Region) {
		return fmt.Errorf("clone.aurora.region %q must be an AWS region such as us-east-1",
			a.Region)
	}
	if a.InstanceClass != "" && !cloneAuroraClass.MatchString(a.InstanceClass) {
		return fmt.Errorf("clone.aurora.instance_class %q must be an RDS instance class such "+
			"as db.r6g.large or db.serverless", a.InstanceClass)
	}
	if a.TTLMinutes < CloneAuroraMinTTLMinutes || a.TTLMinutes > CloneAuroraMaxTTLMinutes {
		return fmt.Errorf("clone.aurora.ttl_minutes must be within %d-%d, got %d",
			CloneAuroraMinTTLMinutes, CloneAuroraMaxTTLMinutes, a.TTLMinutes)
	}
	return nil
}
