// Package provider is the one registry of the PostgreSQL providers pg_sage
// detects: their canonical names, aliases, whether they are managed, and
// how their configuration, logs and preload libraries change. Consumers
// read a Profile instead of keeping their own provider lists.
package provider

import "strings"

// Canonical provider names. Detection, contracts and readiness use these.
const (
	Postgres    = "postgres"
	RDS         = "rds"
	Aurora      = "aurora"
	CloudSQL    = "cloud-sql"
	AlloyDB     = "alloydb"
	Azure       = "azure"
	AzureCosmos = "azure-cosmos"
	Neon        = "neon"
	Supabase    = "supabase"
	Lakebase    = "lakebase"
	// CNPG is a CloudNativePG Cluster: PostgreSQL on Kubernetes whose
	// operator owns postgresql.conf. Detection sees self-managed
	// PostgreSQL; a databases[].cnpg block names the Cluster (market-12).
	CNPG = "cnpg"
	// Unknown is not a profile: it is what a failed probe reports, and it
	// matches nothing so provider-gated work fails closed.
	Unknown = "unknown"
)

// ConfigMechanism is how a provider's instance parameters change.
type ConfigMechanism string

const (
	ConfigAlterSystem           ConfigMechanism = "alter_system"
	ConfigParameterGroup        ConfigMechanism = "parameter_group"
	ConfigClusterParameterGroup ConfigMechanism = "cluster_parameter_group"
	ConfigCloudSQLFlags         ConfigMechanism = "cloudsql_database_flags"
	ConfigAlloyDBFlags          ConfigMechanism = "alloydb_database_flags"
	ConfigServerParameters      ConfigMechanism = "azure_server_parameters"
	// ConfigClusterSpec: the CloudNativePG Cluster's
	// spec.postgresql.parameters; ALTER SYSTEM is disabled.
	ConfigClusterSpec ConfigMechanism = "cnpg_cluster_spec"
	// ConfigGuidanceOnly: pg_sage has no adapter; findings carry the
	// provider's command and no executable SQL.
	ConfigGuidanceOnly ConfigMechanism = "guidance_only"
	// ConfigTenant: the provider owns instance settings; only database,
	// role and session settings change through SQL.
	ConfigTenant ConfigMechanism = "tenant_settings"
)

// Profile describes one provider.
type Profile struct {
	Name            string
	Aliases         []string
	Managed         bool
	ConfigMechanism ConfigMechanism
	// LogAccess is the readiness label for where server logs come from.
	LogAccess string
	// PreloadSteps add a library to shared_preload_libraries ({lib} and
	// {LIB} are the library name as given and upper-cased). Empty means the
	// self-managed ALTER SYSTEM steps apply.
	PreloadSteps []string
}

// Lookup resolves a canonical name or alias, case-folded and trimmed.
// Empty, "unknown" and unrecognised names return false.
func Lookup(name string) (Profile, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return Profile{}, false
	}
	for _, p := range profiles {
		if p.Name == name || contains(p.Aliases, name) {
			return p.clone(), true
		}
	}
	return Profile{}, false
}

// Canonical returns the canonical name for name, or "" when unknown.
func Canonical(name string) string {
	p, _ := Lookup(name)
	return p.Name
}

// IsManaged reports whether name is a managed service where ALTER SYSTEM
// is unavailable. Unknown names are not managed; provider support lists
// still fail closed for them.
func IsManaged(name string) bool {
	p, ok := Lookup(name)
	return ok && p.Managed
}

// PreloadSteps are the provider's steps for adding lib to
// shared_preload_libraries, or nil when the self-managed steps apply.
func PreloadSteps(name, lib string) []string {
	p, ok := Lookup(name)
	if !ok || len(p.PreloadSteps) == 0 {
		return nil
	}
	r := strings.NewReplacer("{lib}", lib, "{LIB}", strings.ToUpper(lib))
	out := make([]string, len(p.PreloadSteps))
	for i, step := range p.PreloadSteps {
		out[i] = r.Replace(step)
	}
	return out
}

// Profiles returns a copy of every profile, in registry order.
func Profiles() []Profile {
	out := make([]Profile, len(profiles))
	for i, p := range profiles {
		out[i] = p.clone()
	}
	return out
}

func (p Profile) clone() Profile {
	p.Aliases = append([]string(nil), p.Aliases...)
	p.PreloadSteps = append([]string(nil), p.PreloadSteps...)
	return p
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
