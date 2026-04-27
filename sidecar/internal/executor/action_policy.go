package executor

import (
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

const (
	PolicyDecisionExecute       = "execute"
	PolicyDecisionQueueApproval = "queue_for_approval"
	PolicyDecisionBlocked       = "blocked"
	PolicyDecisionObserveOnly   = "observe_only"
)

type ActionPolicyContext struct {
	Config              *config.Config
	ExecutionMode       string
	Now                 time.Time
	RampStart           time.Time
	IsReplica           bool
	EmergencyStop       bool
	SafeActionsInFlight int
	SafeActionLimit     int
}

type ActionPolicyDecision struct {
	Decision                  string   `json:"decision"`
	RiskTier                  string   `json:"risk_tier"`
	RequiresApproval          bool     `json:"requires_approval"`
	RequiresMaintenanceWindow bool     `json:"requires_maintenance_window"`
	BlockedReason             string   `json:"blocked_reason,omitempty"`
	Guardrails                []string `json:"guardrails,omitempty"`
	Provider                  string   `json:"provider,omitempty"`
}

func EvaluateActionPolicy(
	contract ActionContract,
	ctx ActionPolicyContext,
) ActionPolicyDecision {
	decision := newPolicyDecision(contract, ctx)
	if blocked := hardBlockReason(contract, ctx, decision.Provider); blocked != "" {
		decision.Decision = PolicyDecisionBlocked
		decision.BlockedReason = blocked
		return decision
	}

	switch contract.BaseRiskTier {
	case "read_only":
		decision.Decision = PolicyDecisionExecute
	case "safe":
		evaluateSafePolicy(&decision, ctx)
	case "moderate", "high":
		evaluateApprovalPolicy(&decision, ctx)
	default:
		decision.Decision = PolicyDecisionBlocked
		decision.BlockedReason = "unknown or prohibited risk tier"
	}
	return decision
}

func newPolicyDecision(
	contract ActionContract,
	ctx ActionPolicyContext,
) ActionPolicyDecision {
	return ActionPolicyDecision{
		Decision:   PolicyDecisionBlocked,
		RiskTier:   contract.BaseRiskTier,
		Guardrails: append([]string(nil), contract.Guardrails...),
		Provider:   normalizedProvider(ctx.Config),
	}
}

func hardBlockReason(
	contract ActionContract,
	ctx ActionPolicyContext,
	provider string,
) string {
	if ctx.EmergencyStop {
		return "emergency stop is active"
	}
	if ctx.IsReplica && contract.BaseRiskTier != "read_only" {
		return "target database is a replica"
	}
	if !providerSupported(provider, contract.ProviderSupport) {
		return fmt.Sprintf("provider %s is not supported", provider)
	}
	return ""
}

func evaluateSafePolicy(decision *ActionPolicyDecision, ctx ActionPolicyContext) {
	cfg := ctx.Config
	if cfg == nil || cfg.Trust.Level == "observation" {
		decision.Decision = PolicyDecisionObserveOnly
		decision.BlockedReason = "policy is observe_only"
		return
	}
	if ctx.ExecutionMode == "approval" || cfg.Trust.Level == "advisory" {
		decision.Decision = PolicyDecisionQueueApproval
		decision.RequiresApproval = true
		return
	}
	if !cfg.Trust.Tier3Safe || rampAge(ctx) < 8*24*time.Hour {
		decision.Decision = PolicyDecisionBlocked
		decision.BlockedReason = "safe-action trust ramp is not satisfied"
		return
	}
	if safeActionLimitReached(ctx) {
		decision.Decision = PolicyDecisionBlocked
		decision.BlockedReason = "safe action concurrency limit reached"
		return
	}
	decision.Decision = PolicyDecisionExecute
}

func evaluateApprovalPolicy(
	decision *ActionPolicyDecision,
	ctx ActionPolicyContext,
) {
	decision.Decision = PolicyDecisionQueueApproval
	decision.RequiresApproval = true
	decision.RequiresMaintenanceWindow = true
	if !inMaintenanceWindowForPolicy(ctx.Config, ctx.Now) {
		decision.BlockedReason = "outside maintenance window"
	}
}

func normalizedProvider(cfg *config.Config) string {
	if cfg == nil || strings.TrimSpace(cfg.CloudEnvironment) == "" {
		return "postgres"
	}
	provider := strings.ToLower(strings.TrimSpace(cfg.CloudEnvironment))
	if provider == "self-managed" {
		return "postgres"
	}
	return provider
}

func providerSupported(provider string, supported []string) bool {
	if len(supported) == 0 {
		return true
	}
	for _, item := range supported {
		if strings.EqualFold(provider, item) {
			return true
		}
	}
	return false
}

func rampAge(ctx ActionPolicyContext) time.Duration {
	now := ctx.Now
	if now.IsZero() {
		now = time.Now()
	}
	if ctx.RampStart.IsZero() {
		return 0
	}
	return now.Sub(ctx.RampStart)
}

func safeActionLimitReached(ctx ActionPolicyContext) bool {
	return ctx.SafeActionLimit > 0 &&
		ctx.SafeActionsInFlight >= ctx.SafeActionLimit
}

func inMaintenanceWindowForPolicy(
	cfg *config.Config,
	now time.Time,
) bool {
	if cfg == nil {
		return false
	}
	window := strings.TrimSpace(cfg.Trust.MaintenanceWindow)
	if window == "" {
		return false
	}
	if now.IsZero() {
		return inMaintenanceWindow(window)
	}
	return inMaintenanceWindowAt(window, now)
}
