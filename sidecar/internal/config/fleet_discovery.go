package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// FleetConfig holds fleet-wide runtime settings (market-06).
type FleetConfig struct {
	// Discovery finds databases, branches and cloud instances to enrol.
	Discovery FleetDiscoveryConfig `yaml:"discovery"`
}

// FleetDiscoveryConfig configures fleet auto-discovery. It applies to a
// meta-db fleet only; standalone mode ignores it. Discovered members are
// enrolled at trust observation and execution approval, and stay there
// until an owner adopts them.
type FleetDiscoveryConfig struct {
	Enabled     bool                    `yaml:"enabled" doc:"List the configured sources every 10 minutes and record the databases they report as candidates. Meta-db fleets only. Default: false."`
	Sources     []DiscoverySourceConfig `yaml:"sources" doc:"Discovery sources: cluster (other databases on a member's server), lakebase (branch endpoints of a project), rds (tagged instances) or neon (branches of Neon projects). Each needs a name and an owner."`
	MaxEnrolled int                     `yaml:"max_enrolled" doc:"Most discovered members enrolled at once, adopted ones included. 0-50; 0 records candidates only. Default: 10."`
	TTLHours    int                     `yaml:"ttl_hours" doc:"Hours an enrolled member runs before it is disabled unless an owner adopts it. 1-720. Default: 72."`
}

// DiscoverySourceConfig is one discovery source.
type DiscoverySourceConfig struct {
	Name             string `yaml:"name" doc:"Source name: lower-case letters, digits, - and _, at most 32. Prefixes the names of the members it enrols."`
	Type             string `yaml:"type" doc:"cluster, lakebase, rds or neon."`
	Owner            string `yaml:"owner" doc:"Who answers for what this source finds (a team or person). Required; copied to every enrolled member."`
	Member           string `yaml:"member" doc:"cluster: the existing member whose server is listed."`
	ReuseCredentials bool   `yaml:"reuse_credentials" doc:"cluster: enrol candidates with the member's role and password. Off: candidates are only recorded. Default: false."`
	Project          string `yaml:"project" doc:"lakebase: the project id whose branch endpoints are listed. neon: the project id whose branches are listed; empty lists every project the clone.neon.api_key key can see."`
	Database         string `yaml:"database" doc:"neon: the database each branch candidate names. Default: neondb."`
	Region           string `yaml:"region" doc:"rds: the AWS region listed."`
	TagKey           string `yaml:"tag_key" doc:"rds: only instances carrying this tag are candidates."`
	TagValue         string `yaml:"tag_value" doc:"rds: the tag's value; empty matches any value."`
}

// Fleet discovery defaults and bounds.
const (
	DefaultDiscoveryMaxEnrolled = 10
	DefaultDiscoveryTTLHours    = 72
	maxDiscoveryEnrolled        = 50 // sage.databases holds at most 50 rows
	maxDiscoveryTTLHours        = 720
)

var discoverySourceName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

func defaultFleetConfig() FleetConfig {
	return FleetConfig{Discovery: FleetDiscoveryConfig{
		MaxEnrolled: DefaultDiscoveryMaxEnrolled, TTLHours: DefaultDiscoveryTTLHours}}
}

// TTL is how long an unadopted enrolled member runs.
func (d FleetDiscoveryConfig) TTL() time.Duration {
	return time.Duration(d.TTLHours) * time.Hour
}

// Validate checks bounds and every source, enabled or not, so a broken
// source is caught before discovery is switched on.
func (d FleetDiscoveryConfig) Validate() error {
	if d.MaxEnrolled < 0 || d.MaxEnrolled > maxDiscoveryEnrolled {
		return fmt.Errorf("fleet.discovery.max_enrolled must be 0-%d, got %d",
			maxDiscoveryEnrolled, d.MaxEnrolled)
	}
	if d.TTLHours < 1 || d.TTLHours > maxDiscoveryTTLHours {
		return fmt.Errorf("fleet.discovery.ttl_hours must be 1-%d, got %d",
			maxDiscoveryTTLHours, d.TTLHours)
	}
	seen := map[string]bool{}
	for i, s := range d.Sources {
		if err := s.validate(); err != nil {
			return fmt.Errorf("fleet.discovery.sources[%d]: %w", i, err)
		}
		if seen[s.Name] {
			return fmt.Errorf("fleet.discovery.sources[%d]: duplicate name %q", i, s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}

func (s DiscoverySourceConfig) validate() error {
	if !discoverySourceName.MatchString(s.Name) {
		return fmt.Errorf("name %q must be 1-32 of a-z, 0-9, - and _", s.Name)
	}
	if strings.TrimSpace(s.Owner) == "" {
		return fmt.Errorf("source %q needs an owner: who answers for what it finds", s.Name)
	}
	required := map[string][]struct{ key, v string }{
		"cluster":  {{"member", s.Member}},
		"lakebase": {{"project", s.Project}},
		"rds":      {{"region", s.Region}, {"tag_key", s.TagKey}},
		"neon":     {},
	}
	fields, ok := required[s.Type]
	if !ok {
		return fmt.Errorf("source %q: type %q must be cluster, lakebase, rds or neon",
			s.Name, s.Type)
	}
	if s.Database != "" && s.Type != "neon" {
		return fmt.Errorf("source %q: database applies to neon sources only", s.Name)
	}
	if s.ReuseCredentials && s.Type != "cluster" {
		return fmt.Errorf("source %q: reuse_credentials applies to cluster sources only",
			s.Name)
	}
	for _, f := range fields {
		if strings.TrimSpace(f.v) == "" {
			return fmt.Errorf("source %q: %s sources need %s", s.Name, s.Type, f.key)
		}
	}
	return nil
}
