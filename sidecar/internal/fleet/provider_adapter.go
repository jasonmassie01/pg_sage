package fleet

type ProviderAdapter struct {
	Provider         string
	Extensions       map[string]string
	LogAccess        string
	Limitations      []string
	SupportedActions map[string]bool
}

func AdapterForProvider(provider string) ProviderAdapter {
	normalized := normalizeProviderName(provider)
	switch normalized {
	case "lakebase":
		return lakebaseAdapter()
	case "neon", "supabase":
		return hostedPostgresAdapter(normalized)
	case "cloud-sql":
		return managedAdapter(normalized, "provider_logging",
			map[string]string{
				"pg_stat_statements": "available",
				"hypopg":             "available",
				"pg_hint_plan":       "provider_parameter_required",
				"auto_explain":       "provider_parameter_required",
			},
			[]string{
				"requires database flag for pg_hint_plan and auto_explain",
				"log access flows through provider logging",
			},
		)
	case "rds":
		return managedAdapter(normalized, "cloudwatch",
			map[string]string{
				"pg_stat_statements": "available",
				"hypopg":             "available",
				"pg_hint_plan":       "parameter_group_required",
				"auto_explain":       "parameter_group_required",
			},
			[]string{
				"parameter group changes may require restart",
				"log access flows through CloudWatch",
			},
		)
	case "aurora":
		return managedAdapter(normalized, "cloudwatch",
			map[string]string{
				"pg_stat_statements": "available",
				"hypopg":             "available",
				"pg_hint_plan":       "cluster_parameter_group_required",
				"auto_explain":       "cluster_parameter_group_required",
			},
			[]string{
				"cluster parameter group changes may require restart",
				"replica topology can change during failover",
			},
		)
	case "alloydb":
		return managedAdapter(normalized, "cloud-logging",
			map[string]string{
				"pg_stat_statements": "available",
				"hypopg":             "available",
				"pg_hint_plan":       "provider_parameter_required",
				"auto_explain":       "provider_parameter_required",
			},
			[]string{
				"provider flags control preload extensions",
				"log access flows through Cloud Logging",
			},
		)
	case "azure":
		return azureAdapter()
	case "postgres":
		return managedAdapter(normalized, "local",
			map[string]string{
				"pg_stat_statements": "unknown",
				"hypopg":             "unknown",
				"pg_hint_plan":       "unknown",
				"auto_explain":       "unknown",
			},
			nil,
		)
	default:
		adapter := managedAdapter(normalized, "unknown",
			defaultExtensionReadiness(), []string{"provider adapter unknown"})
		adapter.SupportedActions = map[string]bool{}
		return adapter
	}
}

// Lakebase only advertises action families verified on the native OAuth path.
// Catalog collection still supplies the endpoint-specific extension evidence.
func lakebaseAdapter() ProviderAdapter {
	adapter := managedAdapter("lakebase", "unavailable", map[string]string{
		"pg_stat_statements": "unknown",
		"hypopg":             "unavailable",
		"pg_hint_plan":       "unknown",
		"auto_explain":       "unavailable",
		"vector":             "unknown",
	}, []string{
		"instance settings and server log files are managed by Lakebase",
		"HypoPG validation is unavailable; historical system-table telemetry requires a configured resource binding and verified delivery",
		"mutations require ownership or the relevant object permission",
		"OAuth requires a direct endpoint; the built-in transaction pooler requires native passwords",
	})
	adapter.SupportedActions = map[string]bool{}
	for _, action := range []string{
		"analyze_table", "vacuum_table", "create_index_concurrently",
		"revert_created_index", "reindex_concurrently", "create_statistics",
		"revert_created_statistics", "alter_database_guc",
	} {
		adapter.SupportedActions[action] = true
	}
	return adapter
}

// azureAdapter describes Azure Database for PostgreSQL flexible server.
// The database owner may ALTER DATABASE ... SET; instance parameters go
// through Azure server parameters (managed config), never ALTER SYSTEM.
func azureAdapter() ProviderAdapter {
	adapter := managedAdapter("azure", "azure_monitor",
		map[string]string{
			"pg_stat_statements": "available",
			"hypopg":             "azure_extensions_allowlist_required",
			"pg_hint_plan":       "azure_extensions_allowlist_required",
			"auto_explain":       "azure_extensions_allowlist_required",
		},
		[]string{
			"the azure.extensions server parameter allow-lists CREATE EXTENSION",
			"preload libraries and server parameters change through Azure; " +
				"some require a restart",
			"log access flows through Azure Monitor diagnostic settings",
		},
	)
	adapter.SupportedActions["alter_database_guc"] = true
	return adapter
}

func hostedPostgresAdapter(provider string) ProviderAdapter {
	extensions := defaultExtensionReadiness()
	extensions["vector"] = "unknown"
	adapter := managedAdapter(provider, "provider_console", extensions, []string{
		"use a direct or session connection for session-dependent operations",
		"extension installation and role privileges must be checked on the target",
		"server configuration and host telemetry are controlled by the provider",
	})
	adapter.SupportedActions["alter_database_guc"] = true
	return adapter
}

func managedAdapter(
	provider string,
	logAccess string,
	extensions map[string]string,
	limitations []string,
) ProviderAdapter {
	return ProviderAdapter{
		Provider:         provider,
		Extensions:       copyExtensions(extensions),
		LogAccess:        logAccess,
		Limitations:      append([]string(nil), limitations...),
		SupportedActions: defaultSupportedActions(),
	}
}

func (a ProviderAdapter) SupportsAction(actionType string) bool {
	if len(a.SupportedActions) == 0 {
		return false
	}
	return a.SupportedActions[actionType]
}

func defaultSupportedActions() map[string]bool {
	actions := []string{
		"analyze_table",
		"vacuum_table",
		"diagnose_lock_blockers",
		"diagnose_runaway_query",
		"diagnose_connection_exhaustion",
		"diagnose_wal_replication",
		"diagnose_standby_conflicts",
		"prepare_sequence_capacity_migration",
		"cancel_backend",
		"terminate_backend",
		"diagnose_freeze_blockers",
		"diagnose_vacuum_pressure",
		"create_index_concurrently",
		"drop_unused_index",
		"set_table_autovacuum",
		"plan_bloat_remediation",
		"reindex_concurrently",
		"prepare_query_rewrite",
		"promote_role_work_mem",
		"create_statistics",
		"prepare_parameterized_query",
		"retire_query_hint",
		"alter_table",
		"ddl_preflight",
	}
	out := make(map[string]bool, len(actions))
	for _, action := range actions {
		out[action] = true
	}
	return out
}

func copyExtensions(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
