package schema

// Agent governance G1 gate composition (AGENTDB-SPEC §6.2.6, §6.11,
// G1-14). All idempotent.
//
// sage.policy gains the agent that proposed a version (an MCP
// propose_policy_change or set_maintenance_policy), its sponsor at the
// time and whether the proposal widens the version it was based on. A
// widening agent proposal needs two people (neither the sponsor), or one
// with a recorded reason under agents.single_operator_mode.
//
// sage.policy_approvals records each person's approval of a proposal;
// single-operator approvals carry review_status 'pending' until someone
// reviews them after the fact (the post-hoc review queue).
const ddlAgentGate = `
/* pg_sage agent_gate v1 */
ALTER TABLE sage.policy ADD COLUMN IF NOT EXISTS proposed_principal_id text;
ALTER TABLE sage.policy ADD COLUMN IF NOT EXISTS proposed_sponsor_id integer;
ALTER TABLE sage.policy ADD COLUMN IF NOT EXISTS widening boolean NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS sage.policy_approvals (
    policy_id        bigint NOT NULL REFERENCES sage.policy(id) ON DELETE CASCADE,
    approver         text NOT NULL CHECK (length(approver) BETWEEN 1 AND 200),
    approver_user_id integer NOT NULL CHECK (approver_user_id > 0),
    reason           text NOT NULL DEFAULT '' CHECK (length(reason) <= 2000),
    single_operator  boolean NOT NULL DEFAULT false,
    review_status    text CHECK (review_status IN ('pending', 'reviewed')),
    decided_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (policy_id, approver)
);
CREATE INDEX IF NOT EXISTS idx_policy_approvals_review
    ON sage.policy_approvals (decided_at) WHERE review_status = 'pending';
`
