package agentdb

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func DecideRequest(req RequestCreate) PolicyDecision {
	isolation := normalizeIsolation(req.IsolationType)
	if strings.TrimSpace(req.TenantID) == "" {
		return decision("deny", "denied", "tenant identity is required")
	}
	if strings.TrimSpace(req.AgentID) == "" {
		return decision("deny", "denied", "agent identity is required")
	}
	if req.Region != "" && len(req.AllowedRegions) > 0 && !regionAllowed(req.Region, req.AllowedRegions) {
		return decision("deny", "denied", "region "+req.Region+" violates provider policy")
	}
	if sensitiveData(req.DataClassification) && strings.TrimSpace(req.MaskingPolicyID) == "" {
		return decision("deny", "denied", "masking policy is required for "+req.DataClassification+" data")
	}
	if sensitiveData(req.DataClassification) {
		return reviewDecision("sensitive data requires review", req.ApprovalSLASeconds)
	}
	switch isolation {
	case "schema", "database":
		return decision("allow", "approved", isolation+" provisioning is supported")
	case "instance":
		if cloudProvider(req.Provider) && req.BudgetUSD <= 0 {
			return reviewDecision("cloud instance requests require budget approval", req.ApprovalSLASeconds)
		}
		return decision("allow", "approved", "cloud instance provisioning creates a plan")
	case "external":
		return decision("review", "requested", "external database requires review")
	case "branch":
		return decision("defer", "deferred", isolation+" isolation is deferred")
	default:
		return decision("deny", "denied", "unsupported isolation type")
	}
}

func BudgetStatus(dep Deployment, summary CostSummary) BudgetDecision {
	if dep.BudgetUSD <= 0 {
		return BudgetDecision{State: "unbudgeted", Action: "none"}
	}
	if summary.TotalUSD >= dep.BudgetUSD {
		return BudgetDecision{State: "hard_limit", Action: "pause"}
	}
	if summary.TotalUSD >= dep.BudgetUSD*0.9 {
		return BudgetDecision{State: "soft_limit", Action: "warn"}
	}
	return BudgetDecision{State: "under_budget", Action: "none"}
}

func CleanupDecisionFor(
	dep Deployment,
	backups []Backup,
	now time.Time,
) CleanupDecision {
	if dep.Status != "archived" {
		if leaseExpired(dep, now) {
			return CleanupDecision{
				Action: "archive",
				Reason: "lease expired without a fresh ping",
			}
		}
		return CleanupDecision{Action: "keep", Reason: "deployment is active"}
	}
	if dep.BackupRequired && !hasRestoreVerifiedBackup(backups) {
		return CleanupDecision{
			Action: "wait_for_verified_backup",
			Reason: "destructive cleanup requires verified restore",
		}
	}
	if cloudProvider(dep.Provider) &&
		dep.ProvisioningLevel == LevelInstance &&
		dep.ProvisioningStatus != "destroyed" {
		return CleanupDecision{
			Action: "wait_for_provider_destroy",
			Reason: "cloud provider resource must be destroyed before row deletion",
		}
	}
	return CleanupDecision{
		Action:    "delete_ready",
		CanDelete: true,
		Reason:    "archive and backup checks passed",
	}
}

func decision(kind, status, reason string) PolicyDecision {
	return PolicyDecision{Decision: kind, Status: status, Reasons: []string{reason}}
}

func reviewDecision(reason string, slaSeconds int) PolicyDecision {
	reasons := []string{reason}
	if slaSeconds > 0 {
		reasons = append(reasons, "approval_sla_seconds="+fmt.Sprint(slaSeconds))
	}
	return PolicyDecision{Decision: "review", Status: "requested", Reasons: reasons}
}

func sensitiveData(classification string) bool {
	switch strings.ToLower(strings.TrimSpace(classification)) {
	case "production", "restricted", "pii", "phi", "pci":
		return true
	default:
		return false
	}
}

func regionAllowed(region string, allowed []string) bool {
	region = strings.ToLower(strings.TrimSpace(region))
	for _, candidate := range allowed {
		if region == strings.ToLower(strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

func normalizeIsolation(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "schema"
	}
	return v
}

func bodyHash(v map[string]any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func idFrom(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func policyReasons(dec PolicyDecision) map[string]any {
	return map[string]any{"decision": dec.Decision, "reasons": dec.Reasons}
}

func leaseExpired(dep Deployment, now time.Time) bool {
	return dep.LeaseExpiresAt != nil && dep.LeaseExpiresAt.Before(now)
}

func hasRestoreVerifiedBackup(backups []Backup) bool {
	for _, backup := range backups {
		if backup.Status == "restore_verified" && backup.RestoreVerifiedAt != nil {
			return true
		}
	}
	return false
}
