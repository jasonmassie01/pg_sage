package whynot

// reasonMapping is where one gate or agent reason lands in the chain: the
// link it fails (or, with allows, the link it shows passing), the plain
// sentence and the fix, or why there is none. Every policy.LedgerReasons
// value has one (reasons_test.go); a new reason without one fails that
// test, so the chain never shows a raw code.
type reasonMapping struct {
	link     LinkName
	allows   bool
	sentence string
	fix      fixSpec
	noFix    string
}

// fixSpec is a fix before the database and the gate detail are known.
// fixCard links the card of the finding asked about; fixWaitDetail waits
// until the time the gate detail names, else links the Held back tab.
type fixSpec struct {
	kind  FixKind
	value string
	note  string
}

const (
	fixCard       FixKind = "card"
	fixWaitDetail FixKind = "wait_detail"
)

// Dashboard pages a fix links to.
const (
	pageTrust    = "#/trust"
	pageFacts    = "#/facts"
	pageAgents   = "#/agents"
	pageHeldBack = "#/actions?tab=decisions"
	pagePending  = "#/actions?tab=pending"
	pageDatabase = "#/manage-databases"
)

var (
	toCard    = fixSpec{kind: fixCard, note: "Approve it on its card."}
	toTrust   = fixSpec{kind: FixUILink, value: pageTrust}
	toAgents  = fixSpec{kind: FixUILink, value: pageAgents}
	waitGate  = fixSpec{kind: fixWaitDetail}
	toAdvisor = fixSpec{kind: FixSetting, value: "trust.level=advisory",
		note: "Raise trust so pg_sage may propose changes to run."}
)

const retries = "pg_sage reads it again on the next cycle; nothing to change"

func allow(link LinkName, sentence string) reasonMapping {
	return reasonMapping{link: link, allows: true, sentence: sentence}
}

func fails(link LinkName, sentence string, fix fixSpec) reasonMapping {
	return reasonMapping{link: link, sentence: sentence, fix: fix}
}

func failsNoFix(link LinkName, sentence, because string) reasonMapping {
	return reasonMapping{link: link, sentence: sentence, noFix: because}
}

var reasonMappings = map[string]reasonMapping{
	// Reasons that let the change run.
	"authorized":            allow(LinkPolicy, "The standing policy would run it now."),
	"operator_approved":     allow(LinkPolicy, "An operator approved it."),
	"deadline_override":     allow(LinkWindow, "Its deadline lets it run outside the window."),
	"narrowing":             allow(LinkPolicy, "It only narrows what pg_sage may do."),
	"narrowing_during_stop": allow(LinkEmergencyStop, "It only narrows, so the stop allows it."),
	"rollback_mitigation":   allow(LinkPolicy, "It rolls back an earlier change."),
	"drop_restore_bypass":   allow(LinkPolicy, "It restores a dropped object."),
	"autonomy_l3":           allow(LinkTrust, "Earned autonomy lets it run unattended."),
	"agent_l3_envelope":     allow(LinkTrust, "The agent's envelope lets it run."),
	// The standing policy.
	"approval_required": fails(LinkPolicy, "Asks first: approval required.", toCard),
	"refused_by_policy": fails(LinkPolicy,
		"The standing policy refuses this kind of change.", toTrust),
	"change_class_not_allowed": fails(LinkPolicy,
		"The standing policy does not allow this class of change.", toTrust),
	"no_typed_contract": failsNoFix(LinkPolicy,
		"pg_sage has no typed contract for this change, so it only advises.",
		"pg_sage runs only changes it has a typed contract for"),
	"unknown_guardrail": failsNoFix(LinkPolicy,
		"The change names a guardrail this release does not know, so it fails closed.",
		"the action contract must be fixed in pg_sage itself"),
	"unknown_risk_tier": failsNoFix(LinkPolicy,
		"The change has a risk tier this release does not know, so it fails closed.",
		"the action contract must be fixed in pg_sage itself"),
	"target_mismatch": failsNoFix(LinkPolicy,
		"The statement does not touch the finding's own object, so it is refused.",
		"pg_sage never runs a statement on another object than its finding's"),
	"sql_validation_degraded": failsNoFix(LinkPolicy,
		"The SQL validator is degraded, so only safe changes run.", retries),
	"policy_unavailable": failsNoFix(LinkPolicy,
		"The standing policy could not be read, so the gate fails closed.", retries),
	"bound_by_fact": fails(LinkPolicy,
		"A confirmed fact about this object forbids the change.",
		fixSpec{kind: FixUILink, value: pageFacts}),
	"facts_unavailable": failsNoFix(LinkPolicy,
		"The confirmed facts could not be read, so the gate fails closed.", retries),
	// fleetpol-02: an exposure fix while the policy's exposure mode is shadow.
	"exposure_shadow": fails(LinkPolicy,
		"Exposure fixes are in shadow mode: pg_sage records the change it would make.",
		fixSpec{kind: FixSetting, value: "exposure.mode=enforce",
			note: "Set it in the standing policy document to let exposure fixes run."}),
	"agent_proposal_recorded": fails(LinkPolicy,
		"The agent's proposal is recorded and waits for review.",
		fixSpec{kind: FixUILink, value: pagePending}),
	// The window.
	"outside_maintenance_window": fails(LinkWindow,
		"Outside the maintenance window.", waitGate),
	"window_closing": fails(LinkWindow,
		"The maintenance window closes too soon to start it.", waitGate),
	"agent_change_freeze": fails(LinkWindow,
		"A change freeze holds the agent's changes.", toAgents),
	// Budgets.
	"budget_exceeded": fails(LinkBudget, "The storage budget is spent.", waitGate),
	"blast_radius_exceeded": fails(LinkBudget,
		"The blast-radius budget for this window is spent.", waitGate),
	"rate_limit_exceeded": fails(LinkBudget,
		"The change budget for this window is spent.", waitGate),
	"agent_rate":   fails(LinkBudget, "The agent's rate limit is spent.", toAgents),
	"agent_budget": fails(LinkBudget, "The agent's budget is spent.", toAgents),
	// Privilege, provider and replica.
	"replica_mutation": failsNoFix(LinkPrivilege,
		"This database is a read-only replica, so pg_sage changes nothing on it.",
		"a replica cannot accept changes; pg_sage acts on the primary"),
	"provider_unsupported": failsNoFix(LinkPrivilege,
		"The provider manages this setting, so pg_sage cannot change it.",
		"the provider owns it; change it in the provider's console"),
	"agent_no_pitr": failsNoFix(LinkPrivilege,
		"No point-in-time recovery is configured, so the agent may not change it.",
		"enable point-in-time recovery with the provider or backup tool"),
	"agent_capability": fails(LinkPrivilege,
		"The agent does not hold the capability this change needs.", toAgents),
	"agent_classification": fails(LinkPrivilege,
		"The data classification forbids the agent this change.", toAgents),
	"agent_env_ceiling": fails(LinkPrivilege,
		"The environment's ceiling forbids the agent this change.", toAgents),
	// Trust and autonomy.
	"observe_only": fails(LinkTrust,
		"Trust level is observation: pg_sage watches and runs nothing.", toAdvisor),
	"unknown_trust_level": fails(LinkTrust,
		"The trust level is not one pg_sage knows, so it fails closed.", toAdvisor),
	"trust_ramp_not_satisfied": fails(LinkTrust,
		"Trust is still ramping up for this kind of change.", toTrust),
	"autonomy_handoff": fails(LinkTrust,
		"Earned autonomy hands this change to an operator.", toTrust),
	"autonomy_level": fails(LinkTrust,
		"Its autonomy level does not let it run unattended.", toTrust),
	"autonomy_downgraded": fails(LinkTrust,
		"Its autonomy was lowered after a bad outcome.", toTrust),
	"autonomy_unavailable": failsNoFix(LinkTrust,
		"The autonomy ledger could not be read, so the gate fails closed.", retries),
	"agent_level0": fails(LinkTrust, "The agent is at level 0: it may only watch.",
		toAgents),
	"agent_frozen":      fails(LinkTrust, "The agent is frozen.", toAgents),
	"agent_unsponsored": fails(LinkTrust, "The agent has no sponsor.", toAgents),
	"agent_retired":     fails(LinkTrust, "The agent is retired.", toAgents),
	"agent_lease_expired": fails(LinkTrust,
		"The agent's lease expired.", toAgents),
	"agent_governance_unavailable": failsNoFix(LinkTrust,
		"Agent governance could not be read, so the gate fails closed.", retries),
	// Changes in flight.
	"awaiting_verification": fails(LinkVerification,
		"An earlier change on this object is still being verified.", waitGate),
	"ddl_conflict": failsNoFix(LinkVerification,
		"Another schema change on this object is running.",
		"it clears when the other change ends"),
	"custodian_backoff": fails(LinkVerification,
		"The custodian backed off after repeated failures; an operator reviews it.",
		fixSpec{kind: FixUILink, value: pageHeldBack}),
	// Hard stops.
	"emergency_stop": fails(LinkEmergencyStop,
		"Emergency stop is on.", fixSpec{kind: FixUILink, value: pageDatabase,
			note: "Release it in the header to resume."}),
	"executor_disabled": fails(LinkEmergencyStop,
		"The executor is switched off for this database.",
		fixSpec{kind: FixUILink, value: pageDatabase}),
}

// mappingFor is the reason's mapping; a reason no table names (a
// sentence written by an older release) is a policy failure as written.
func mappingFor(reason string) (reasonMapping, bool) {
	m, ok := reasonMappings[reason]
	return m, ok
}
