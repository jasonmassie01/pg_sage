package agentdb

import (
	"context"
	"strings"
	"time"
)

type ReadinessDisabledReason struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type ProviderRuntimeCheck struct {
	CredentialError    error
	ConfigurationError error
}

type ProviderReadinessOptions struct {
	Now              time.Time
	Registry         *RunnerRegistry
	RuntimeEnabled   bool
	GlobalPolicy     *LivePolicyLayer
	ProviderPolicies map[string]*LivePolicyLayer
	RuntimeChecks    map[string]ProviderRuntimeCheck
}

func ProviderReadinessList(
	ctx context.Context,
	options ...ProviderReadinessOptions,
) []ProviderReadiness {
	_ = ctx
	if len(options) == 0 {
		return providerReadinessWithoutRuntime()
	}
	opts := options[0]
	return []ProviderReadiness{
		localProviderReadiness(),
		providerReadiness(opts, ProviderAWSRDS, "AWS RDS", "aws_sdk"),
		providerReadiness(opts, ProviderGCPCloudSQL, "GCP Cloud SQL", "cloudsql_admin_api"),
		providerReadiness(
			opts,
			ProviderDatabricksLakebase,
			"Databricks Lakebase",
			"databricks_api",
		),
	}
}

func providerReadinessWithoutRuntime() []ProviderReadiness {
	disabled := ReadinessDisabledReason{
		Code:   "runtime_dependencies_missing",
		Detail: "runtime registry and effective policies were not supplied",
	}
	return []ProviderReadiness{
		localProviderReadiness(),
		blockedProviderReadiness(ProviderAWSRDS, "AWS RDS", "aws_sdk", disabled),
		blockedProviderReadiness(
			ProviderGCPCloudSQL, "GCP Cloud SQL", "cloudsql_admin_api", disabled,
		),
		blockedProviderReadiness(
			ProviderDatabricksLakebase,
			"Databricks Lakebase",
			"databricks_api",
			disabled,
		),
	}
}

func localProviderReadiness() ProviderReadiness {
	return ProviderReadiness{
		Provider: ProviderLocalPostgres, Label: "Local Postgres",
		Interface: "pg_sage", Found: true,
		Detail: "uses the active pg_sage PostgreSQL connection",
	}
}

func blockedProviderReadiness(
	provider string,
	label string,
	iface string,
	reason ReadinessDisabledReason,
) ProviderReadiness {
	return ProviderReadiness{
		Provider: provider, Label: label, Interface: iface,
		DisabledReasons: []ReadinessDisabledReason{reason}, Detail: reason.Detail,
	}
}

func providerReadiness(
	opts ProviderReadinessOptions,
	provider string,
	label string,
	iface string,
) ProviderReadiness {
	reasons := runtimeReadinessReasons(opts, provider)
	providerPolicy := opts.ProviderPolicies[provider]
	reasons = append(reasons, policyReadinessReasons(opts, providerPolicy)...)
	result := resolveReadinessPolicy(opts, provider, providerPolicy)
	if !result.Decision.Allowed && len(reasons) == 0 {
		reasons = append(reasons, ReadinessDisabledReason{
			Code:   "effective_policy_denied",
			Detail: strings.Join(result.Decision.DisabledReasons, "; "),
		})
	}
	row := ProviderReadiness{
		Provider: provider, Label: label, Interface: iface,
		Found:           len(reasons) == 0 && result.Decision.Allowed,
		DisabledReasons: uniqueReadinessReasons(reasons),
		PolicyHash:      result.PolicyHash,
	}
	if providerPolicy != nil {
		row.PolicyVersion = providerPolicy.Version
	}
	if row.Found {
		row.Detail = "live runner and effective policy are ready"
	} else {
		row.Detail = readinessDetail(row.DisabledReasons)
	}
	return row
}

func runtimeReadinessReasons(
	opts ProviderReadinessOptions,
	provider string,
) []ReadinessDisabledReason {
	reasons := []ReadinessDisabledReason{}
	if !opts.RuntimeEnabled {
		reasons = append(reasons, ReadinessDisabledReason{
			Code: "runtime_disabled", Detail: "runtime live provisioning is disabled",
		})
	}
	var runner ProviderRunner
	var err error
	if opts.Registry != nil {
		runner, err = opts.Registry.ForProvider(provider)
	}
	if err != nil || runner == nil || runner.Name() == "dry_run" {
		reasons = append(reasons, ReadinessDisabledReason{
			Code: "runner_unavailable", Detail: "a live provider runner is not registered",
		})
	}
	check := opts.RuntimeChecks[provider]
	if check.CredentialError != nil {
		reasons = append(reasons, ReadinessDisabledReason{
			Code: "credentials_invalid", Detail: "provider credentials failed validation",
		})
	}
	if check.ConfigurationError != nil {
		reasons = append(reasons, ReadinessDisabledReason{
			Code: "runtime_config_invalid", Detail: "provider runtime configuration is invalid",
		})
	}
	return reasons
}

func policyReadinessReasons(
	opts ProviderReadinessOptions,
	provider *LivePolicyLayer,
) []ReadinessDisabledReason {
	reasons := []ReadinessDisabledReason{}
	if opts.GlobalPolicy == nil {
		reasons = append(reasons, ReadinessDisabledReason{
			Code: "global_policy_missing", Detail: "global AgentDB policy is missing",
		})
	} else if !opts.GlobalPolicy.Policy.LiveProvisioningEnabled {
		reasons = append(reasons, ReadinessDisabledReason{
			Code: "global_disabled", Detail: "global AgentDB live provisioning is disabled",
		})
	}
	if provider == nil {
		return append(reasons, ReadinessDisabledReason{
			Code: "provider_policy_missing", Detail: "persisted provider policy is missing",
		})
	}
	if !provider.Policy.ProviderEnabled {
		reasons = append(reasons, ReadinessDisabledReason{
			Code: "provider_disabled", Detail: "persisted provider policy is disabled",
		})
	}
	if provider.Err != nil {
		reasons = append(reasons, ReadinessDisabledReason{
			Code: "policy_store_error", Detail: "persisted provider policy is unreadable",
		})
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if provider.ValidationTTL > 0 &&
		(provider.ValidatedAt.IsZero() || now.After(
			provider.ValidatedAt.Add(provider.ValidationTTL),
		)) {
		reasons = append(reasons, ReadinessDisabledReason{
			Code: "policy_stale", Detail: "persisted provider policy validation is stale",
		})
	}
	return reasons
}

func resolveReadinessPolicy(
	opts ProviderReadinessOptions,
	provider string,
	providerPolicy *LivePolicyLayer,
) EffectiveLivePolicyResult {
	if opts.GlobalPolicy == nil || providerPolicy == nil {
		return EffectiveLivePolicyResult{
			Decision: LivePolicyDecision{
				DisabledReasons: []string{"required policy layer is missing"},
			},
		}
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	region := readinessProbeValue(intersectAllowlist(
		opts.GlobalPolicy.Policy.AllowedRegions,
		providerPolicy.Policy.AllowedRegions,
	))
	planHash := "readiness-probe"
	return ResolveEffectiveLiveProvisionPolicy(EffectiveLivePolicyInput{
		Now: now,
		Runtime: &RuntimeLiveCapability{
			Enabled:         opts.RuntimeEnabled,
			RunnerAvailable: runnerAvailable(opts.Registry, provider),
			Policy:          opts.GlobalPolicy.Policy,
		},
		Global: opts.GlobalPolicy, Provider: providerPolicy,
		Authorization: &LiveOperationAuthorization{
			Allowed: true, Provider: provider, Operation: ProvisionOpCreate,
			DeploymentID: "readiness-probe", PlanHash: planHash,
			ExpiresAt: now.Add(time.Minute),
		},
		Operation: ProvisionOpCreate, DeploymentID: "readiness-probe",
		PlanHash: planHash,
		Request: LiveProvisionRequest{
			Provider: provider, Region: region, TTLSeconds: 1,
			EstimatedCostUSD: 0.01,
		},
	})
}

func runnerAvailable(registry *RunnerRegistry, provider string) bool {
	if registry == nil {
		return false
	}
	runner, err := registry.ForProvider(provider)
	return err == nil && runner != nil && runner.Name() != "dry_run"
}

func readinessProbeValue(values []string) string {
	if len(values) == 0 || values[0] == "*" {
		return "readiness-probe"
	}
	return values[0]
}

func uniqueReadinessReasons(
	reasons []ReadinessDisabledReason,
) []ReadinessDisabledReason {
	seen := map[string]bool{}
	out := make([]ReadinessDisabledReason, 0, len(reasons))
	for _, reason := range reasons {
		if reason.Code != "" && !seen[reason.Code] {
			seen[reason.Code] = true
			out = append(out, reason)
		}
	}
	return out
}

func readinessDetail(reasons []ReadinessDisabledReason) string {
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, reason.Detail)
	}
	return strings.Join(parts, "; ")
}
