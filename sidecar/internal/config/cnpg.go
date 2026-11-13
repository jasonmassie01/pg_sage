package config

import (
	"fmt"
	"regexp"
)

// CNPGConfig names the CloudNativePG Cluster behind one database
// (market-12). With it the provider is cnpg: ALTER SYSTEM is never used,
// instance settings become Cluster spec proposals, and the sidecar reads
// the Cluster (get only) with its service account.
type CNPGConfig struct {
	Cluster   string `yaml:"cluster" doc:"CloudNativePG Cluster of this database; makes the provider cnpg."`
	Namespace string `yaml:"namespace" doc:"Kubernetes namespace of the CloudNativePG Cluster."`
}

// Enabled reports a configured block.
func (c CNPGConfig) Enabled() bool { return c != CNPGConfig{} }

var cnpgName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// validateCNPG checks a cnpg block: both names, RFC 1123 labels, on a
// database detection may call self-managed, with no cloud_resource.
func validateCNPG(field, provider string, c CNPGConfig, r CloudResourceConfig) error {
	if !c.Enabled() {
		return nil
	}
	if c.Cluster == "" || c.Namespace == "" {
		return fmt.Errorf("%s.cnpg: set cluster and namespace together", field)
	}
	for _, f := range [][2]string{{"cluster", c.Cluster}, {"namespace", c.Namespace}} {
		if !cnpgName.MatchString(f[1]) {
			return fmt.Errorf("%s.cnpg.%s %q is not a Kubernetes name (lower-case letters, "+
				"digits and hyphens, at most 63)", field, f[0], f[1])
		}
	}
	if provider != "" && provider != "self-managed" {
		return fmt.Errorf("%s.cnpg names a CloudNativePG Cluster, which runs self-managed "+
			"PostgreSQL, but provider %q is set; remove one of them", field, provider)
	}
	if len(r.clouds()) > 0 {
		return fmt.Errorf("%s: set cnpg or cloud_resource, not both", field)
	}
	return nil
}
