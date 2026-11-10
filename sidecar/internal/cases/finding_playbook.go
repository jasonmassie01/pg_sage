package cases

import "time"

// runawayPolicyOwnsSignalReason blocks a case cancel of a runaway query:
// the executor's runaway policy already signals that backend under its
// own identity checks, and a second signal from a case would race it.
const runawayPolicyOwnsSignalReason = "the executor's runaway policy owns " +
	"cancel and terminate for this backend"

// findingPlaybookCandidates maps the analyzer and executor finding
// categories whose incident-style playbooks lived behind signal ids RCA
// never emits (playbooks-02). ok is false for every other category.
func findingPlaybookCandidates(f SourceFinding) ([]ActionCandidate, bool) {
	switch f.Category {
	case "sequence_exhaustion":
		return sequenceCapacityCandidates(f), true
	case "inactive_slot", "replication_lag":
		// Read-only: an inactive slot's pg_drop_replication_slot SQL is
		// never proposed; dropping a slot loses its consumer's position.
		return []ActionCandidate{findingDiagnosticCandidate(f,
			"diagnose_wal_replication",
			[]string{"verify WAL retention, slot status, and replay lag"})}, true
	case "runaway_query":
		expires := evidenceTime(f).Add(15 * time.Minute)
		return []ActionCandidate{
			findingDiagnosticCandidate(f, "diagnose_runaway_query",
				runawayVerificationPlan()),
			blockedSignalCandidate("cancel_backend", runawayPolicyOwnsSignalReason,
				expires, findingDiagnosticConfidence),
		}, true
	default:
		return nil, false
	}
}

// findingDiagnosticConfidence is the confidence of a read-only diagnostic
// derived from a deterministic analyzer or executor measurement.
const findingDiagnosticConfidence = 0.80

// findingDiagnosticCandidate proposes the action's probe script
// (diagnosticSQL).
func findingDiagnosticCandidate(f SourceFinding, actionType string,
	verification []string) ActionCandidate {
	expires := evidenceTime(f).Add(15 * time.Minute)
	return ActionCandidate{
		ActionType:       actionType,
		RiskTier:         "safe",
		Confidence:       findingDiagnosticConfidence,
		ProposedSQL:      diagnosticSQL(actionType),
		ExpiresAt:        &expires,
		OutputModes:      []string{"execute", "script"},
		RollbackClass:    "not_applicable",
		VerificationPlan: verification,
	}
}
