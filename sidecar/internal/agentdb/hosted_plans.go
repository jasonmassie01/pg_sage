package agentdb

import "os"

func hostedProvisionPlan(req RegisterRequest, profile SizeProfile) ProvisionPlan {
	mode := param(profile, "mode", "branch")
	out := plan(req, "cloud_api", []string{"cloud_api", req.Provider, "create_" + mode,
		resourceName(req.DeploymentID)}, "Use the provider management API with scoped credentials.")
	out.Notes = append(out.Notes,
		"Verify organization plan and resource quota; a request plan field cannot enforce free billing.",
		"Branch creation copies schema only; pg_sage retains TTL cleanup ownership.",
		"Creation is asynchronous; retain the returned provider resource ID for status and teardown.")
	return out
}

func hostedLifecycleCommand(dep Deployment, action string) (ProviderCommand, error) {
	if dep.ProviderResourceID == "" {
		return ProviderCommand{}, ErrInvalid
	}
	switch action {
	case "status", "destroy", "backup_check":
		return ProviderCommand{Tool: "cloud_api", Args: []string{
			"cloud_api", dep.Provider, action, dep.ProviderResourceID}}, nil
	default:
		return ProviderCommand{}, ErrInvalid
	}
}

func hostedSizeProfile(provider, mode string) SizeProfile {
	return SizeProfile{ProfileID: provider + "_" + mode, Provider: provider,
		ProvisioningLevel: LevelInstance, Name: provider + " " + mode,
		Description: "Provider-managed resource; verify plan, quotas and current usage pricing",
		StorageGB:   1, MonthlyBudgetUSD: 5,
		ProviderParams: map[string]any{"mode": mode, "public_ip": true, "storage_gb": 1},
	}
}

func registerHostedFromEnv(registry *RunnerRegistry) {
	providers := []struct{ provider, enable, token string }{
		{ProviderNeon, "PG_SAGE_ENABLE_NEON_RUNNER", "PG_SAGE_NEON_API_KEY"},
		{ProviderSupabase, "PG_SAGE_ENABLE_SUPABASE_RUNNER", "PG_SAGE_SUPABASE_ACCESS_TOKEN"},
	}
	for _, entry := range providers {
		if os.Getenv(entry.enable) != "1" || os.Getenv(entry.token) == "" {
			continue
		}
		client := HostedHTTPClient{Provider: entry.provider,
			TokenFunc: staticToken(os.Getenv(entry.token))}
		if entry.provider == ProviderSupabase {
			client.PasswordFunc = staticToken(os.Getenv("PG_SAGE_SUPABASE_DATABASE_PASSWORD"))
		}
		registry.Register(NewHostedRunner(entry.provider, client))
	}
}
