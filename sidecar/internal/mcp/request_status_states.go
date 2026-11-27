package mcp

// The state an agent sees for each store's row (mcp-01b). States:
// pending (a person has not decided), approved, running, halted, executed,
// verified, completed, rejected, failed, expired, cancelled, reverted,
// rolled_back, recorded, held, blocked, resolved and unknown. Final says
// nothing more will happen. decided_by_kind is person, policy (the
// standing policy decided, no person) or system (expiry, voiding); an
// agent never decides, so it is never "agent".

const (
	byPerson = "person"
	byPolicy = "policy"
	bySystem = "system"
)

func record(state, by, outcome string, final bool) RequestRecord {
	return RequestRecord{State: state, DecidedByKind: by, Outcome: outcome, Final: final}
}

func unrecognised(status string) RequestRecord {
	return record(stateUnknown, "", "status "+quoteStatus(status)+
		" is not one pg_sage knows; call get_request_status later", false)
}

func quoteStatus(status string) string {
	if status == "" {
		return "(none)"
	}
	return "\"" + status + "\""
}

// queueState maps sage.action_queue: decided_by is the person who
// decided; an approved or executed row without one was the policy's.
func queueState(status, verification string, decidedBy *int) RequestRecord {
	by := byPolicy
	if decidedBy != nil {
		by = byPerson
	}
	switch status {
	case "pending":
		return record("pending", "", "waiting for a person in Actions", false)
	case "scheduled", "approved":
		return record("approved", by, "approved; pg_sage runs it through the executor", false)
	case "executed":
		return executedQueueState(verification, by)
	case "rejected":
		if decidedBy == nil {
			by = bySystem
		}
		return record("rejected", by, "rejected; nothing runs", true)
	case "expired":
		return record("expired", bySystem, "expired before anyone decided", true)
	case "failed":
		return record("failed", by, "the change failed; see the action log", true)
	case "rolled_back":
		return record("rolled_back", by, "the change ran and was rolled back", true)
	}
	return unrecognised(status)
}

func executedQueueState(verification, by string) RequestRecord {
	switch verification {
	case "verified":
		return record("verified", by, "ran and verified", true)
	case "failed":
		return record("failed", by, "ran, and verification failed", true)
	}
	return record("executed", by, "ran; verification is "+quoteStatus(verification), false)
}

// planState maps sage.plan; only a person approves or rejects a plan.
func planState(status string) RequestRecord {
	switch status {
	case "draft", "proposed":
		return record("pending", "", "waiting for a person to approve the plan", false)
	case "approved":
		return record("approved", byPerson, "approved; its steps run in order", false)
	case "running":
		return record("running", byPerson, "its steps are running", false)
	case "halted":
		return record("halted", byPerson, "halted mid-change; a person resumes, reverts "+
			"or cancels it", false)
	case "completed":
		return record("completed", byPerson, "every step ran and was judged", true)
	case "reverted":
		return record("reverted", byPerson, "reverted", true)
	case "rejected":
		return record("rejected", byPerson, "rejected; nothing runs", true)
	case "voided":
		return record("cancelled", bySystem, "voided: what it was based on changed", true)
	case "cancelled":
		return record("cancelled", byPerson, "cancelled", true)
	}
	return unrecognised(status)
}

// grantState maps sage.guard_grant_requests.
func grantState(status string, decided bool) RequestRecord {
	by := bySystem
	if decided {
		by = byPerson
	}
	switch status {
	case "pending":
		return record("pending", "", "waiting for an operator", false)
	case "approved":
		return record("approved", byPerson, "granted; agent_whoami lists the grant", true)
	case "denied":
		return record("rejected", byPerson, "denied; nothing was granted", true)
	case "expired":
		return record("expired", bySystem, "expired before anyone decided", true)
	case "failed":
		return record("failed", by, "approved, but the grant could not be applied", true)
	case "recorded":
		return record("recorded", byPolicy, "recorded as a proposal; it is never approved",
			true)
	}
	return unrecognised(status)
}

// policyState maps sage.policy rows that began as proposals.
func policyState(status string, ratified bool) RequestRecord {
	switch {
	case status == "proposed":
		return record("pending", "", "waiting for an admin to ratify it", false)
	case ratified && (status == "active" || status == "ratified" || status == "superseded"):
		return record("approved", byPerson, "ratified by an admin", true)
	case status == "superseded":
		return record("cancelled", bySystem, "superseded before it was ratified", true)
	case status == "rejected":
		return record("rejected", byPerson, "rejected; the policy is unchanged", true)
	}
	return unrecognised(status)
}

// decisionState maps a sage.decision row with no approval item: nothing
// waits on a person, so it is final for a waiter.
func decisionState(verdict string, resolved, executed bool) RequestRecord {
	switch {
	case verdict == "":
		return unrecognised(verdict)
	case resolved:
		return record("resolved", byPolicy, "resolved by a later decision", true)
	case verdict == "execute" && executed:
		return record("executed", byPolicy, "the standing policy ran it", true)
	case verdict == "execute":
		return record("approved", byPolicy, "the standing policy allowed it", true)
	case verdict == "blocked":
		return record("blocked", byPolicy, "blocked by the policy check", true)
	case verdict == "parked" || verdict == "queue_approval" || verdict == "observe_only" ||
		verdict == "recommended":
		return record("held", byPolicy, "held back by the policy check; nothing waits on "+
			"a person (see get_ledger)", true)
	}
	return unrecognised(verdict)
}
