package config

import (
	"fmt"
	"regexp"
)

// CloneNeonBranch is the clone.provider of Neon branch rehearsals
// (lab-05b).
const CloneNeonBranch = "neon_branch"

// Bounds of the clone.neon block: a leaked branch holds production data
// for at most a day; the endpoint autoscales from 0.25 CU to at most 8.
const (
	CloneNeonMaxTTLMinutes = 1440
	CloneNeonMinCU         = 0.25
	CloneNeonMaxCU         = 8.0
)

var (
	cloneNeonProjectID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	cloneNeonParent    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)
)

// CloneNeonConfig is the Neon project rehearsal branches are made in
// (clone.provider neon_branch).
type CloneNeonConfig struct {
	ProjectID     string  `yaml:"project_id" doc:"Neon project id rehearsal branches are created in (the monitored database's project)."`
	ParentBranch  string  `yaml:"parent_branch" doc:"Branch id (br-...) or name a rehearsal branches from. Default: the branch of the monitored endpoint, found by its host."`
	APIKey        string  `yaml:"api_key" doc:"Neon API key that creates and deletes the rehearsal branches; a project-scoped key where possible. Without it rehearsals are recommend-only. Also SAGE_CLONE_NEON_API_KEY." secret:"true"`
	EndpointMaxCU float64 `yaml:"endpoint_max_cu" doc:"Autoscaling maximum, in compute units, of the read-write endpoint on a rehearsal branch, 0.25 to 8. Default: 1."`
	TTLMinutes    int     `yaml:"ttl_minutes" doc:"Expiry Neon sets on every rehearsal branch, in minutes, 1 to 1440: the backstop if pg_sage cannot delete it. Default: 60."`
}

// validate checks the block; it runs only when neon_branch is the
// provider. An empty API key is valid (CloneProviderConfig.Warnings).
func (n CloneNeonConfig) validate() error {
	if n.ProjectID == "" {
		return fmt.Errorf("clone.neon.project_id is required for clone.provider neon_branch")
	}
	if len(n.ProjectID) > 63 || !cloneNeonProjectID.MatchString(n.ProjectID) {
		return fmt.Errorf("clone.neon.project_id %q must be a Neon project id: 1-63 "+
			"lowercase letters, digits or hyphens", n.ProjectID)
	}
	if n.ParentBranch != "" && !cloneNeonParent.MatchString(n.ParentBranch) {
		return fmt.Errorf("clone.neon.parent_branch %q must be a branch id (br-...) or a "+
			"name of letters, digits and . _ / - (at most 256)", n.ParentBranch)
	}
	if n.TTLMinutes < 1 || n.TTLMinutes > CloneNeonMaxTTLMinutes {
		return fmt.Errorf("clone.neon.ttl_minutes must be within 1-%d, got %d",
			CloneNeonMaxTTLMinutes, n.TTLMinutes)
	}
	if !(n.EndpointMaxCU >= CloneNeonMinCU && n.EndpointMaxCU <= CloneNeonMaxCU) {
		return fmt.Errorf("clone.neon.endpoint_max_cu must be within %v-%v, got %v",
			CloneNeonMinCU, CloneNeonMaxCU, n.EndpointMaxCU)
	}
	return nil
}
