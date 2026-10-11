package decommission

import (
	"sort"
	"strings"
)

// Credential is a provider secret or grant the removed provisioner used.
// Kind "env" is an environment variable (Set reports whether it still holds
// a value; the value itself is never read into the inventory); kind "iam" is
// a grant from the provider runbooks, which pg_sage cannot observe.
type Credential struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Set      bool   `json:"set"`
	Action   string `json:"action"`
}

// envDSNPrefix names the agent-database DSN variables fleet sync resolved.
const envDSNPrefix = "PG_SAGE_AGENTDB_"

type envCredential struct{ name, provider, action string }

const (
	revoke = "unset it, then revoke or rotate the credential at the provider"
	unset  = "unset it; it no longer has any effect"
)

// envCredentials are every variable the removed runners and gates read.
var envCredentials = []envCredential{
	{"PG_SAGE_LIVE_PROVISIONING", "all", unset},
	{"PG_SAGE_AGENTDB_LOCAL_PROVISIONING", providerLocal, unset},
	{"PG_SAGE_ENABLE_AWS_RDS_RUNNER", providerRDS, unset},
	{"PG_SAGE_AWS_REGION", providerRDS, unset},
	{"PG_SAGE_ENABLE_GCP_CLOUDSQL_RUNNER", providerCloudSQL, unset},
	{"PG_SAGE_GCP_ACCESS_TOKEN", providerCloudSQL, revoke},
	{"PG_SAGE_GCP_ACCESS_TOKEN_TTL_SECONDS", providerCloudSQL, unset},
	{"PG_SAGE_GCP_TOKEN_SOURCE", providerCloudSQL, unset},
	{"PG_SAGE_GCP_METADATA_URL", providerCloudSQL, unset},
	{"PG_SAGE_GCP_PROJECT", providerCloudSQL, unset},
	{"PG_SAGE_GCP_REGION", providerCloudSQL, unset},
	{"PG_SAGE_ENABLE_LAKEBASE_RUNNER", providerLakebase, unset},
	{"PG_SAGE_DATABRICKS_HOST", providerLakebase, unset},
	{"PG_SAGE_DATABRICKS_TOKEN", providerLakebase, revoke},
	{"PG_SAGE_ENABLE_NEON_RUNNER", providerNeon, unset},
	{"PG_SAGE_NEON_API_KEY", providerNeon, revoke},
	{"PG_SAGE_ENABLE_SUPABASE_RUNNER", providerSupabase, unset},
	{"PG_SAGE_SUPABASE_ACCESS_TOKEN", providerSupabase, revoke},
	// The Supabase master secret: the database password new projects got.
	{"PG_SAGE_SUPABASE_DATABASE_PASSWORD", providerSupabase, revoke},
}

// iamGrants are the minimum grants the provider runbooks asked for.
var iamGrants = []Credential{
	{Kind: "iam", Provider: providerRDS, Name: "rds:CreateDBInstance, " +
		"rds:DescribeDBInstances, rds:DeleteDBInstance, rds:AddTagsToResource",
		Action: "remove the grants from the identity pg_sage ran as"},
	{Kind: "iam", Provider: providerCloudSQL, Name: "cloudsql.instances.create, " +
		"cloudsql.instances.get, cloudsql.instances.delete, cloudsql.operations.get",
		Action: "remove the role from the service account pg_sage ran as"},
	{Kind: "iam", Provider: providerLakebase, Name: "Lakebase project read, branch " +
		"create and branch delete", Action: "revoke the service principal's token"},
}

// credentials lists the known variables, every PG_SAGE_AGENTDB_* variable in
// the environment and every env: reference a deployment recorded, then the
// runbook grants. Only names and whether a value is set are reported.
func credentials(
	lookupEnv func(string) (string, bool),
	environ []string,
	secretRefs []string,
) []Credential {
	fromEnviron := map[string]string{}
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		fromEnviron[name] = value
	}
	byName := map[string]Credential{}
	add := func(name, provider, action string) {
		if _, seen := byName[name]; seen || name == "" {
			return
		}
		value, ok := lookupEnv(name)
		if !ok {
			value = fromEnviron[name]
		}
		byName[name] = Credential{Kind: "env", Name: name, Provider: provider,
			Set: value != "", Action: action}
	}
	for _, c := range envCredentials {
		add(c.name, c.provider, c.action)
	}
	for name := range fromEnviron {
		if strings.HasPrefix(name, envDSNPrefix) {
			add(name, "agent database", revoke)
		}
	}
	for _, ref := range secretRefs {
		if name, ok := strings.CutPrefix(ref, "env:"); ok {
			add(strings.TrimPrefix(name, "//"), "agent database", revoke)
		}
	}
	out := make([]Credential, 0, len(byName)+len(iamGrants))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return append(out, iamGrants...)
}

// setCredentialNames are the environment variables that still hold a value.
func setCredentialNames(creds []Credential) []string {
	var names []string
	for _, c := range creds {
		if c.Kind == "env" && c.Set {
			names = append(names, c.Name)
		}
	}
	sort.Strings(names)
	return names
}
