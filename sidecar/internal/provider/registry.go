package provider

const (
	restartStep = "Restart the server (shared_preload_libraries applies at start)."
	rebootStep  = "Then reboot the instance (the setting applies at start)."
)

// profiles is the registry. Every name provider detection can return has
// a profile here except Unknown (see cmd/pg_sage_sidecar knownProviders).
var profiles = []Profile{
	{Name: Postgres, Aliases: []string{"self-managed", "postgresql"},
		ConfigMechanism: ConfigAlterSystem, LogAccess: "local"},
	{Name: RDS, Aliases: []string{"aws", "aws-rds"}, Managed: true,
		ConfigMechanism: ConfigParameterGroup, LogAccess: "cloudwatch",
		PreloadSteps: []string{"In the instance's DB parameter group, add {lib} to " +
			"shared_preload_libraries.", rebootStep}},
	{Name: Aurora, Aliases: []string{"aurora-postgresql"}, Managed: true,
		ConfigMechanism: ConfigClusterParameterGroup, LogAccess: "cloudwatch",
		PreloadSteps: []string{"In the instance's DB cluster parameter group, add {lib} to " +
			"shared_preload_libraries.", rebootStep}},
	{Name: CloudSQL, Aliases: []string{"cloudsql", "gcp", "cloud sql", "gcp-cloud-sql"},
		Managed: true, ConfigMechanism: ConfigCloudSQLFlags, LogAccess: "provider_logging",
		PreloadSteps: []string{"Cloud SQL preloads {lib}; no database flag is needed to " +
			"enable it. Its settings are database flags (gcloud sql instances patch " +
			"INSTANCE --database-flags=...)."}},
	{Name: AlloyDB, Managed: true, ConfigMechanism: ConfigAlloyDBFlags,
		LogAccess: "cloud-logging",
		PreloadSteps: []string{"AlloyDB preloads {lib}; no database flag is needed to enable " +
			"it. Its settings are database flags (gcloud alloydb instances update INSTANCE " +
			"--database-flags=...)."}},
	{Name: Azure, Aliases: []string{"azure-flexible", "azure-single", "azure-postgres"},
		Managed: true, ConfigMechanism: ConfigServerParameters, LogAccess: "azure_monitor",
		PreloadSteps: []string{"In the server parameters, add {LIB} to azure.extensions and " +
			"to shared_preload_libraries (az postgres flexible-server parameter set).",
			restartStep}},
	{Name: AzureCosmos, Managed: true, ConfigMechanism: ConfigGuidanceOnly,
		LogAccess: "azure_monitor",
		PreloadSteps: []string{"In the coordinator's server parameters, add {LIB} to " +
			"azure.extensions and to shared_preload_libraries (az cosmosdb postgres " +
			"configuration coordinator update).", "Restart the cluster " +
			"(shared_preload_libraries applies at start)."}},
	{Name: Neon, Managed: true, ConfigMechanism: ConfigTenant, LogAccess: "provider_console",
		PreloadSteps: []string{"{lib} is preloaded on neon."}},
	{Name: Supabase, Managed: true, ConfigMechanism: ConfigTenant,
		LogAccess: "provider_console", PreloadSteps: []string{"{lib} is preloaded on supabase."}},
	{Name: Lakebase, Managed: true, ConfigMechanism: ConfigTenant, LogAccess: "unavailable",
		PreloadSteps: []string{"Lakebase manages shared_preload_libraries; verify the " +
			"extension is supported by this endpoint and ask a database owner to enable it " +
			"in this database."}},
	{Name: CNPG, Aliases: []string{"cloudnative-pg", "cloudnativepg"}, Managed: true,
		ConfigMechanism: ConfigClusterSpec, LogAccess: "kubernetes",
		PreloadSteps: []string{"In the CloudNativePG Cluster manifest, add {lib} to " +
			"spec.postgresql.shared_preload_libraries (kubectl patch cluster, or the " +
			"GitOps repository that owns the Cluster).", "The operator applies it with a " +
			"rolling restart: the replicas, then the primary (restart or switchover)."}},
}
