// gateLabels: the policy gate's verdicts and reasons in words (ux-05b
// wins 6 and 8). Every value the gate, the decision ledger and the
// executor can serve is listed in gateValues.json, generated from the Go
// constants (internal/executor/gate_values_test.go); workflow.test.js
// fails when one of them has no label here. A reason that holds a change
// back says what to do next. React renders every label as text.

const VERDICT_LABELS = Object.freeze({
  execute: 'Allowed to run',
  queue_approval: 'Waiting for approval',
  queue_for_approval: 'Waiting for approval',
  park: 'Parked',
  parked: 'Parked',
  blocked: 'Blocked',
  observe_only: 'Observe only',
})

// The standing gate's reasons (policy.LedgerReasons, the gate's half).
const GATE_REASONS = {
  authorized: 'Allowed by the standing policy.',
  operator_approved: 'Approved by an operator.',
  deadline_override: 'Allowed outside the window because a deadline is close.',
  narrowing: 'Allowed: the change only takes access away.',
  narrowing_during_stop: 'Allowed during a stop: the change only takes access away.',
  rollback_mitigation: 'Allowed: it undoes an earlier change.',
  drop_restore_bypass: 'Allowed: it restores an index pg_sage dropped.',
  autonomy_l3: 'Allowed by the autonomy level.',
  approval_required: "This change needs a person's approval. Review it under Needs your review.",
  refused_by_policy: 'The standing policy hands this change to a person. ' +
    'Review it under Needs your review.',
  sql_validation_degraded: 'The SQL could not be fully checked, so a person must approve it.',
  autonomy_handoff: 'The autonomy level hands this change to a person.',
  no_typed_contract: 'pg_sage has no safe procedure for this kind of change, ' +
    'so it will not run it on its own.',
  budget_exceeded: 'Change budget exceeded. It runs when the budget frees; ' +
    'raise the budget in the standing policy to allow more.',
  blast_radius_exceeded: 'The change touches more than the blast-radius limit allows.',
  rate_limit_exceeded: 'Rate limit exceeded. It is retried when the limit frees.',
  awaiting_verification: 'Waiting for an earlier change to be verified. ' +
    'It is retried once verification finishes.',
  ddl_conflict: 'Another schema change holds a lock on this object. It is retried next cycle.',
  executor_disabled: 'The executor is turned off. Turn it on in Settings to allow changes.',
  emergency_stop: 'Emergency stop is on. Release it in the header to resume.',
  replica_mutation: 'This connection is a read replica, so it cannot change the database. ' +
    'Connect the primary to apply it.',
  policy_unavailable: 'The standing policy could not be read, so nothing runs. ' +
    'Check the policy in Settings.',
  unknown_guardrail: 'The change names a guardrail pg_sage does not know, so it is held.',
  unknown_trust_level: 'The trust level is not recognised, so nothing runs. ' +
    'Check the trust setting.',
  unknown_risk_tier: 'The risk tier is not recognised, so the change is held.',
  change_class_not_allowed: 'The standing policy does not allow this kind of change.',
  trust_ramp_not_satisfied: 'pg_sage has not yet earned the trust this change needs.',
  outside_maintenance_window: 'Outside the maintenance window. It runs in the next window.',
  window_closing: 'The maintenance window closes too soon to start it. ' +
    'It waits for the next window.',
  provider_unsupported: 'pg_sage cannot run this action for this connection.',
  target_mismatch: 'The statement changes a different object than the finding names, ' +
    'so it is blocked.',
  bound_by_fact: 'A recorded fact about this database rules the change out.',
  facts_unavailable: 'The database facts could not be read, so the change is held.',
  autonomy_unavailable: 'The autonomy settings could not be read, so the change is held.',
  observe_only: 'Observe only: pg_sage reports this and does not change the database.',
  autonomy_level: 'The autonomy level does not let pg_sage run this on its own.',
  autonomy_downgraded: 'Autonomy was lowered after a problem, so a person must approve it.',
}

// The agent governance reasons and the executor's own escalation.
const AGENT_REASONS = {
  agent_level0: 'The agent may only observe.',
  agent_proposal_recorded: "The agent's proposal was recorded for review.",
  agent_l3_envelope: "Allowed inside the agent's approved limits.",
  agent_frozen: 'The agent is frozen. Unfreeze it on the Agents page to resume.',
  agent_unsponsored: 'The agent has no sponsor. Assign one on the Agents page.',
  agent_retired: 'The agent is retired.',
  agent_env_ceiling: 'This environment does not allow the agent to make this change.',
  agent_capability: 'The agent lacks the capability for this change.',
  agent_classification: 'The data classification does not let this agent touch it.',
  agent_change_freeze: 'A change freeze is on for agents.',
  agent_no_pitr: 'Point-in-time recovery is not set up, so agents may not make this change.',
  agent_rate: 'The agent reached its rate limit. It is retried when the limit frees.',
  agent_budget: 'The agent used up its change budget.',
  agent_lease_expired: "The agent's lease expired. The agent must renew it to continue.",
  agent_governance_unavailable: 'Agent governance could not be read, so the change is held.',
  custodian_backoff: 'Repeated attempts failed, so the change was handed to a person.',
}

// Reasons the case and approval surfaces add to the gate's.
const SURFACE_REASONS = {
  observation_mode: 'Database changes are disabled in Observation mode.',
  observation: 'Database changes are disabled in Observation mode.',
  insufficient_privilege: 'This connection lacks permission for the proposed change.',
  permission_denied: 'This connection lacks permission for the proposed change.',
  learning_baseline: 'Waiting for sufficient disk-load history.',
  maintenance_window: 'Waiting for an allowed maintenance window.',
  provider_managed_setting: 'The provider manages this setting, so pg_sage cannot change it. ' +
    "Change it in the provider's console.",
  // internal/cases/expiry.go (cases-10).
  evidence_expired: 'Evidence expired; waiting for a fresh observation.',
  recommendation_reverted: 'pg_sage applied and reverted this change before, ' +
    'so it will not propose it again.',
  recommendation_abandoned: 'The retries for this change are used up, ' +
    'so pg_sage will not propose it again.',
}

const REASON_LABELS = Object.freeze({ ...GATE_REASONS, ...AGENT_REASONS, ...SURFACE_REASONS })

// A reason code is one lower-case snake_case token; anything else is a
// sentence a surface wrote itself and is shown as written.
const REASON_CODE = /^[a-z][a-z0-9]*(_[a-z0-9]+)*$/

export function hasReasonLabel(value) {
  return typeof value === 'string' && Object.hasOwn(REASON_LABELS, value)
}

// reasonLabel words a reason: its label, the sentence as written, or
// Unknown reason (<code>) for a code this dashboard does not know; '' for
// none, so callers can fall back.
export function reasonLabel(value) {
  const text = typeof value === 'string' ? value.trim() : ''
  if (!text) return ''
  if (hasReasonLabel(text)) return REASON_LABELS[text]
  return REASON_CODE.test(text) ? `Unknown reason (${text})` : text
}

// reasonText is reasonLabel for a line that must say something.
export function reasonText(value) {
  return reasonLabel(value) || 'No reason recorded.'
}

export function verdictLabel(value) {
  const text = typeof value === 'string' ? value.trim() : ''
  if (!text) return 'No verdict recorded'
  return Object.hasOwn(VERDICT_LABELS, text) ? VERDICT_LABELS[text] : `Unknown verdict (${text})`
}
