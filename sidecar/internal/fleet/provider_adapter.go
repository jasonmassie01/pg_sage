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
		"diagnose_freeze_blockers",
		"create_index_concurrently",
		"drop_unused_index",
		"prepare_query_rewrite",
		"promote_role_work_mem",
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
