package agentdb

import "strings"

type LiveProvisionPolicy struct {
	LiveProvisioningEnabled bool     `json:"live_provisioning_enabled"`
	ProviderEnabled         bool     `json:"provider_enabled"`
	Provider                string   `json:"provider"`
	AllowedRegions          []string `json:"allowed_regions"`
	AllowedAccounts         []string `json:"allowed_accounts"`
	AllowedProjects         []string `json:"allowed_projects"`
	AllowedWorkspaces       []string `json:"allowed_workspaces"`
	AllowPublicIP           bool     `json:"allow_public_ip"`
	RequireBackupBeforeDrop bool     `json:"require_backup_before_destroy"`
	MaxTTLSeconds           int      `json:"max_ttl_seconds"`
	MaxEstimatedCostUSD     float64  `json:"max_estimated_cost_usd"`
}

type LiveProvisionRequest struct {
	Provider             string
	Region               string
	Account              string
	Project              string
	Workspace            string
	TTLSeconds           int
	PublicIP             bool
	EstimatedCostUSD     float64
	EstimatedCostDoubled bool
	Approved             bool
	AdminOverrideReason  string
}

type LivePolicyDecision struct {
	Allowed         bool     `json:"allowed"`
	RequiresReview  bool     `json:"requires_review"`
	Reasons         []string `json:"reasons"`
	DisabledReasons []string `json:"disabled_reasons"`
}

func EvaluateLiveProvisionPolicy(
	policy LiveProvisionPolicy,
	req LiveProvisionRequest,
) LivePolicyDecision {
	decision := LivePolicyDecision{Allowed: true}
	if !policy.LiveProvisioningEnabled {
		return deny(decision, "live provisioning is disabled globally")
	}
	if !policy.ProviderEnabled {
		return deny(decision, "provider is disabled")
	}
	if req.TTLSeconds <= 0 {
		return deny(decision, "ttl is required for live provisioning")
	}
	if policy.MaxTTLSeconds > 0 && req.TTLSeconds > policy.MaxTTLSeconds {
		return deny(decision, "ttl exceeds provider policy")
	}
	if req.PublicIP && !policy.AllowPublicIP {
		return deny(decision, "public ip is denied by policy")
	}
	if !allowedValue(policy.AllowedRegions, req.Region) {
		return deny(decision, "region is not allowlisted")
	}
	if req.Account != "" && !allowedValue(policy.AllowedAccounts, req.Account) {
		return deny(decision, "account is not allowlisted")
	}
	if req.Project != "" && !allowedValue(policy.AllowedProjects, req.Project) {
		return deny(decision, "project is not allowlisted")
	}
	if req.Workspace != "" && !allowedValue(policy.AllowedWorkspaces, req.Workspace) {
		return deny(decision, "workspace is not allowlisted")
	}
	if policy.MaxEstimatedCostUSD > 0 &&
		req.EstimatedCostUSD > policy.MaxEstimatedCostUSD {
		return deny(decision, "estimated ttl cost exceeds budget")
	}
	if req.EstimatedCostDoubled && policy.MaxEstimatedCostUSD > 0 &&
		req.EstimatedCostUSD > policy.MaxEstimatedCostUSD*0.5 && !req.Approved {
		decision.RequiresReview = true
		decision.Reasons = append(decision.Reasons, "low-confidence estimate needs review")
	}
	if req.AdminOverrideReason != "" {
		decision.Reasons = append(decision.Reasons, "admin override recorded")
	}
	return decision
}

func deny(decision LivePolicyDecision, reason string) LivePolicyDecision {
	decision.Allowed = false
	decision.DisabledReasons = append(decision.DisabledReasons, reason)
	return decision
}

func allowedValue(allowed []string, value string) bool {
	value = strings.TrimSpace(value)
	if len(allowed) == 0 {
		return false
	}
	for _, candidate := range allowed {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		if value != "" && strings.EqualFold(candidate, value) {
			return true
		}
	}
	return false
}

func rejectSecretSettings(settings map[string]any) error {
	for key, value := range settings {
		if sensitiveKey(key) {
			return ErrInvalid
		}
		if nested, ok := value.(map[string]any); ok {
			if err := rejectSecretSettings(nested); err != nil {
				return err
			}
		}
	}
	return nil
}
