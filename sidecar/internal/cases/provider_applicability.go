package cases

import (
	"regexp"
	"strings"
)

const lakebaseManagedSettingReason = "Lakebase manages instance settings. This generic PostgreSQL ALTER SYSTEM recommendation is not an applicable SQL procedure for this connection. Review the supported Lakebase configuration controls; no replacement setting has been validated."

// ApplyProviderApplicability withholds generic server-setting advice after the
// connection's provider is known. This is a presentation safeguard; executor
// policy and SQL validation remain responsible for authorizing any execution.
// Source evidence is retained, but must not be presented as a runnable fix.
func ApplyProviderApplicability(c *Case, provider string) {
	if c == nil {
		return
	}
	withheld := false
	for i := range c.ActionCandidates {
		candidate := &c.ActionCandidates[i]
		knownProvider := provider
		if (strings.TrimSpace(knownProvider) == "" || strings.EqualFold(strings.TrimSpace(knownProvider), "unknown")) && candidate.PolicyDecision != nil {
			knownProvider = candidate.PolicyDecision.Provider
		}
		if !strings.EqualFold(strings.TrimSpace(knownProvider), "lakebase") || !candidateHasAlterSystem(*candidate) {
			continue
		}
		candidate.ProposedSQL = ""
		candidate.ScriptOutput = nil
		candidate.OutputModes = nil
		candidate.RollbackClass = "not_applicable"
		candidate.RecoveryDetail = "No change is proposed: the generic server-setting procedure is inapplicable to Lakebase."
		candidate.BlockedReason = lakebaseManagedSettingReason
		candidate.RequiresApproval = false
		candidate.RequiresMaintenanceWindow = false
		candidate.VerificationPlan = []string{"Review supported Lakebase configuration controls and validate any alternative separately."}
		if candidate.PolicyDecision != nil {
			candidate.PolicyDecision.Decision = "blocked"
			candidate.PolicyDecision.BlockedReason = "provider_managed_setting"
			candidate.PolicyDecision.RequiresApproval = false
			candidate.PolicyDecision.RequiresMaintenanceWindow = false
		}
		withheld = true
	}
	if withheld {
		c.Why = lakebaseManagedSettingReason
	}
}

func candidateHasAlterSystem(candidate ActionCandidate) bool {
	if alterSystemDirective.MatchString(candidate.ProposedSQL) {
		return true
	}
	if candidate.ScriptOutput == nil {
		return false
	}
	if alterSystemDirective.MatchString(candidate.ScriptOutput.MigrationSQL) || alterSystemDirective.MatchString(candidate.ScriptOutput.RollbackSQL) {
		return true
	}
	for _, sql := range candidate.ScriptOutput.VerificationSQL {
		if alterSystemDirective.MatchString(sql) {
			return true
		}
	}
	return false
}

// Comments can separate SQL keywords. Conservatively withholding a directive
// in a comment is preferable to exporting provider-inapplicable instructions.
var alterSystemDirective = regexp.MustCompile(`(?is)\bALTER(?:\s|/\*.*?\*/|--[^\r\n]*(?:\r?\n|$))+SYSTEM\b`)
