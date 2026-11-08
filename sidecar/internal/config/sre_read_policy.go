package config

import "fmt"

// SREReadPolicyConfig is trusted startup configuration, not an API/model grant.
// Auto preserves standalone behavior and selects database scope on Lakebase.
type SREReadPolicyConfig struct {
	Scope          string   `yaml:"scope" doc:"SRE observation scope: auto (database on Lakebase, legacy standalone elsewhere), database, or cluster. Restart required. Default: auto."`
	AllowedSources []string `yaml:"allowed_sources" doc:"Optional narrowing list of SRE source IDs; cannot widen database scope. Nil uses every eligible source; an empty explicit list denies all. Restart required."`
}

func (r SREReadPolicyConfig) validate() error {
	switch r.Scope {
	case "", "auto", "database", "cluster":
	default:
		return fmt.Errorf("sre.read_policy.scope must be auto, database or cluster")
	}
	seen := map[string]bool{}
	for _, id := range r.AllowedSources {
		if id == "" || seen[id] {
			return fmt.Errorf("empty or repeated sre.read_policy source")
		}
		seen[id] = true
	}
	return nil
}
