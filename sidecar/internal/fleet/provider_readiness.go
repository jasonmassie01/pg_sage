package fleet

import prov "github.com/pg-sage/sidecar/internal/provider"

// adapterReadinessSpec is a provider's extension readiness and limitations
// shown in fleet readiness. A nil extensions map means every extension is
// unknown until the catalog is read.
type adapterReadinessSpec struct {
	extensions  map[string]string
	limitations []string
}

var hostedPostgresReadiness = adapterReadinessSpec{
	extensions: map[string]string{"pg_stat_statements": "unknown", "hypopg": "unknown",
		"pg_hint_plan": "unknown", "auto_explain": "unknown", "vector": "unknown"},
	limitations: []string{
		"use a direct or session connection for session-dependent operations",
		"extension installation and role privileges must be checked on the target",
		"server configuration and host telemetry are controlled by the provider",
	},
}

// adapterReadiness holds each registry provider's readiness detail.
var adapterReadiness = map[string]adapterReadinessSpec{
	prov.Postgres: {},
	prov.CloudSQL: {extensions: providerParameterExtensions("provider_parameter_required"),
		limitations: []string{"requires database flag for pg_hint_plan and auto_explain",
			"log access flows through provider logging"}},
	prov.RDS: {extensions: providerParameterExtensions("parameter_group_required"),
		limitations: []string{"parameter group changes may require restart",
			"log access flows through CloudWatch"}},
	prov.Aurora: {extensions: providerParameterExtensions("cluster_parameter_group_required"),
		limitations: []string{"cluster parameter group changes may require restart",
			"replica topology can change during failover"}},
	prov.AlloyDB: {extensions: providerParameterExtensions("provider_parameter_required"),
		limitations: []string{"provider flags control preload extensions",
			"log access flows through Cloud Logging"}},
	// Azure flexible server: the database owner may ALTER DATABASE ... SET;
	// instance parameters go through Azure server parameters, never ALTER SYSTEM.
	prov.Azure: {extensions: azureExtensions(), limitations: []string{
		"the azure.extensions server parameter allow-lists CREATE EXTENSION",
		"preload libraries and server parameters change through Azure; " +
			"some require a restart",
		"log access flows through Azure Monitor diagnostic settings",
	}},
	// Cosmos DB for PostgreSQL (Citus): pg_sage has no adapter for its server
	// parameters, so they stay guidance-only.
	prov.AzureCosmos: {extensions: azureExtensions(), limitations: []string{
		"server parameters are guidance-only (Citus coordinator); set them with " +
			"az cosmosdb postgres configuration",
		"the azure.extensions allow-list controls CREATE EXTENSION",
		"log access flows through Azure Monitor diagnostic settings",
	}},
	// CloudNativePG (market-12): plain PostgreSQL whose operator owns
	// postgresql.conf; the image decides which extensions exist.
	prov.CNPG: {extensions: map[string]string{"pg_stat_statements": "unknown",
		"hypopg": "unknown", "pg_hint_plan": "cluster_spec_required",
		"auto_explain": "cluster_spec_required"}, limitations: []string{
		"instance settings change through the Cluster's spec.postgresql.parameters; " +
			"ALTER SYSTEM is disabled and pg_sage never patches the Cluster",
		"preload libraries go in spec.postgresql.shared_preload_libraries; a restart-bound " +
			"change is a rolling restart that restarts or switches over the primary",
		"server logs are the instance pods' output (kubectl logs)",
	}},
	prov.Neon:     hostedPostgresReadiness,
	prov.Supabase: hostedPostgresReadiness,
	// Lakebase families come from the contracts verified on the native OAuth
	// path; catalog collection supplies the endpoint-specific extension evidence.
	prov.Lakebase: {extensions: map[string]string{"pg_stat_statements": "unknown",
		"hypopg": "unavailable", "pg_hint_plan": "unknown", "auto_explain": "unavailable",
		"vector": "unknown"}, limitations: []string{
		"instance settings and server log files are managed by Lakebase",
		"HypoPG validation is unavailable; historical system-table telemetry requires a " +
			"configured resource binding and verified delivery",
		"mutations require ownership or the relevant object permission",
		"OAuth requires a direct endpoint; the built-in transaction pooler requires " +
			"native passwords",
	}},
}

func providerParameterExtensions(preload string) map[string]string {
	return map[string]string{"pg_stat_statements": "available", "hypopg": "available",
		"pg_hint_plan": preload, "auto_explain": preload}
}

func azureExtensions() map[string]string {
	const allowlist = "azure_extensions_allowlist_required"
	return map[string]string{"pg_stat_statements": "available", "hypopg": allowlist,
		"pg_hint_plan": allowlist, "auto_explain": allowlist}
}
