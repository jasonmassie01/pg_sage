package policy

// ledgerReasons are the reasons a decision can be recorded with in
// sage.decision: the gate's own (this package), the agent decider's steps
// D1-D10 (agentguard/decide) and the core agent reasons D1 and D2 record
// (agentguard). The decide package imports this one, so its values are
// listed, not referenced; ledger_reasons_test.go pins the list against
// every declared constant, so it cannot drift (policy-07).
var ledgerReasons = []Reason{
	// The standing gate.
	ReasonAuthorized, ReasonOperatorApproved, ReasonDeadlineOverride, ReasonNarrowing,
	ReasonNarrowingDuringStop, ReasonRollbackMitigation, ReasonDropRestore,
	ReasonAutonomyL3, ReasonApprovalRequired, ReasonRefusedByPolicy,
	ReasonSQLValidationDegraded, ReasonAutonomyHandoff, ReasonNoTypedContract,
	ReasonBudgetExceeded, ReasonBlastRadiusExceeded, ReasonRateLimitExceeded,
	ReasonAwaitingVerification, ReasonDDLConflict, ReasonExecutorDisabled,
	ReasonEmergencyStop, ReasonReplicaMutation, ReasonPolicyUnavailable,
	ReasonUnknownGuardrail, ReasonUnknownTrustLevel, ReasonUnknownRiskTier,
	ReasonChangeClassNotAllowed, ReasonTrustRampNotSatisfied,
	ReasonOutsideMaintenanceWindow, ReasonProviderUnsupported, ReasonTargetMismatch,
	ReasonBoundByFact, ReasonFactsUnavailable, ReasonAutonomyUnavailable,
	ReasonObserveOnly, ReasonAutonomyLevel, ReasonAutonomyDowngraded,
	ReasonAgentLevel0, ReasonAgentProposalRecorded, ReasonAgentL3Envelope,
	// Agent governance (agentguard and agentguard/decide).
	"agent_frozen", "agent_unsponsored", "agent_retired", "agent_env_ceiling",
	"agent_capability", "agent_classification", "agent_change_freeze", "agent_no_pitr",
	"agent_rate", "agent_budget", "agent_lease_expired", "agent_governance_unavailable",
}

var ledgerReasonSet = func() map[Reason]bool {
	set := make(map[Reason]bool, len(ledgerReasons))
	for _, reason := range ledgerReasons {
		set[reason] = true
	}
	return set
}()

// LedgerReasons returns every reason a decision can be recorded with, in a
// new slice the caller may change.
func LedgerReasons() []Reason {
	return append([]Reason(nil), ledgerReasons...)
}

// IsLedgerReason reports whether value is exactly one of LedgerReasons.
func IsLedgerReason(value string) bool {
	return ledgerReasonSet[Reason(value)]
}
