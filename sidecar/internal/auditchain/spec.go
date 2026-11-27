// Package auditchain makes pg_sage's audit tables tamper-evident (E2,
// spec §6.17): every audit row carries the hash of its predecessor, and a
// verifier detects edits, deletions and reordering.
//
// A chain is one audit table. Each write appends a link to
// sage.audit_chain_link: an insert (I), a change of the row's mutable
// state (U), a delete (D) or a truncate (T). A link hashes its predecessor's
// hash, its position, the operation, the row id, a hash of the row's sealed
// columns (what happened, never rewritten) and the row's state (outcome
// columns pg_sage legitimately updates later). The inserted row also
// carries its own link in chain_seq, chain_prev_hash and chain_hash.
//
// Links are appended by deferred constraint triggers, so the chain is
// linked at commit: the advisory lock that orders writers is the last lock
// a transaction takes, and is held only while it commits. That keeps
// concurrent writers deadlock-free and the chain gap-free (a rolled-back
// write never takes a position), and makes chain order commit order, which
// the SIEM exporter uses as its cursor.
//
// The chain proves integrity against anyone who edits rows without also
// recomputing every later hash. A full verification also finds rows whose
// own link is gone (links deleted from the tail, or the whole chain), which
// the walk alone cannot see: it trusts the head it reads from the same
// table. Someone who can rewrite the whole chain, rows included, or a
// restore that takes the chain back, is caught by the off-box copy: the
// SIEM export carries each link's hash, and before each pass the exporter
// checks that the link at every sink's cursor still has the hash it
// exported. A chain that went back stops for that sink as "regressed"
// (never skipping the positions new links reuse) and raises
// audit_chain_regressed until an admin re-anchors the cursor.
//
// Two marker links carry no row (audit-03b). A signed checkpoint (C)
// signs, with the evidence-signing key, the hash of a head a clean
// verification reached, and reaches the SIEM like any link. A prune (P),
// opt-in, records that the links below a floor were removed once every
// sink held a signed checkpoint above them; verification of a pruned chain
// starts at the floor and rests on that checkpoint's signature.
package auditchain

import (
	"fmt"
	"regexp"
	"strings"
)

// Spec describes one chained table. Column expressions are SQL over the
// row alias %[1]s; Sealed must start with the row id.
type Spec struct {
	Chain   string   // chain name, also the link table's chain key
	Table   string   // qualified table, sage.<name>
	Version int      // payload version recorded on every link
	Sealed  []string // columns fixed at insert
	State   []string // columns legitimately updated later; empty = append-only
	// Optional installs the chain only when the table exists (tables
	// another workstream creates).
	Optional bool
}

var (
	chainName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
	tableName = regexp.MustCompile(`^sage\.[a-z][a-z0-9_]{0,50}$`)
)

func (s Spec) validate() error {
	switch {
	case !chainName.MatchString(s.Chain):
		return fmt.Errorf("auditchain: spec chain %q is not a lower-case identifier", s.Chain)
	case !tableName.MatchString(s.Table):
		return fmt.Errorf("auditchain: spec %s table %q is not sage.<name>", s.Chain, s.Table)
	case s.Version < 1:
		return fmt.Errorf("auditchain: spec %s version must be positive", s.Chain)
	case len(s.Sealed) == 0:
		return fmt.Errorf("auditchain: spec %s has no sealed columns", s.Chain)
	}
	return nil
}

// epochUS renders a timestamptz column as integer microseconds since the
// epoch: exact, and independent of the session's TimeZone and DateStyle.
func epochUS(col string) string {
	return "(extract(epoch FROM %[1]s." + col + ") * 1000000)::bigint"
}

func col(name string) string { return "%[1]s." + name }

// ActionLog chains sage.action_log. What was executed and approved is
// sealed; the outcome columns the executor, verifier and value ledger
// update afterwards are state.
var ActionLog = Spec{
	Chain: "action_log", Table: "sage.action_log", Version: 1,
	Sealed: []string{col("id"), epochUS("executed_at"), col("action_type"),
		col("finding_id"), col("sql_executed"), col("rollback_sql"), col("before_state"),
		col("approved_by"), epochUS("approved_at"), col("decision_id"), col("database_id")},
	State: []string{col("outcome"), col("rollback_reason"), epochUS("measured_at"),
		col("after_state"), col("verification_id"), col("justification"),
		col("toil_minutes_saved"), col("toil_model_version")},
}

// AuthAudit chains sign-ins and user administration (E1). Append-only.
var AuthAudit = Spec{
	Chain: "auth_audit", Table: "sage.auth_audit", Version: 1,
	Sealed: []string{col("id"), col("event"), col("actor_user_id"), col("target_user_id"),
		col("detail"), epochUS("created_at"), col("source_ip")},
}

// ConfigAudit chains configuration changes. Append-only.
var ConfigAudit = Spec{
	Chain: "config_audit", Table: "sage.config_audit", Version: 1,
	Sealed: []string{col("id"), col("key"), col("old_value"), col("new_value"),
		col("database_id"), col("changed_by"), epochUS("changed_at"),
		col("changed_by_actor")},
}

// GuardQueryAudit chains brokered agent queries (§7). The table belongs to
// the core workstream, so the chain installs once it exists.
var GuardQueryAudit = Spec{
	Chain: "guard_query_audit", Table: "sage.guard_query_audit", Version: 1,
	Sealed: []string{col("id"), col("database_id") + "::text", col("principal_id"),
		col("task_id"), col("envelope_hash"), col("verdict"), col("reason"),
		col("row_count"), col("classes"), epochUS("at")},
	Optional: true,
}

// PGAuditEvents chains the pgaudit records pg_sage correlated to its own
// actions and to agent principals (§6.17). Append-only.
var PGAuditEvents = Spec{
	Chain: "pgaudit_events", Table: "sage.guard_pgaudit_events", Version: 1,
	Sealed: []string{col("id"), col("database_name"), epochUS("logged_at"),
		col("session_id"), col("pid"), col("db_user"), col("application"),
		col("principal_id"), col("action_id"), col("audit_type"), col("statement_id"),
		col("substatement_id"), col("class"), col("command"), col("object_type"),
		col("object_name"), col("statement"), col("correlated_by")},
}

// FactEvents chains the history of fact decisions (facts-07): who
// proposed, confirmed, rejected, declared or expired a binding fact.
// Append-only; rejecting or expiring a confirmed fact lifts a guard.
var FactEvents = Spec{
	Chain: "fact_events", Table: "sage.fact_events", Version: 1,
	Sealed: []string{col("id"), col("fact_id"), epochUS("at"), col("actor"), col("verb"),
		col("from_status"), col("to_status"), col("fact_hash"), col("note")},
}

// DecisionLedger chains the policy gate's decisions (audit-11): the
// authority a change ran under. What was decided about, at which tier and
// under which policy is sealed. The verdict is state, with the reason,
// evidence and links pg_sage updates later: the executor legitimately
// turns an execute into the block it became (vacuum admission, the
// maintenance precheck). repeat_count and last_seen_at are telemetry and
// stay unchained, so a parked repeat appends nothing.
var DecisionLedger = Spec{
	Chain: "decision", Table: "sage.decision", Version: 1,
	Sealed: []string{col("id"), col("database_id"), col("feature"), col("intent"),
		col("target_objects"), col("policy_version"), col("policy_doc_sha256"),
		col("risk_tier"), col("evidence_id"), col("fingerprint"), col("principal_id"),
		col("task_id"), col("artifact_hash"), epochUS("created_at")},
	State: []string{col("verdict"), col("reason"), col("evidence"), col("guardrails"),
		col("off_window_ok"), col("deadline_kind"), epochUS("deadline_hard_at"),
		col("queue_id"), col("action_log_id"), epochUS("resolved_at"), col("candidate_key")},
	Optional: true,
}

// PolicyApprovals chains each person's approval of a policy version
// (audit-11). The approval is sealed; its post-hoc review is state. The
// table lives where the policy does (the control database in fleet mode).
var PolicyApprovals = Spec{
	Chain: "policy_approvals", Table: "sage.policy_approvals", Version: 1,
	Sealed: []string{col("id"), col("policy_id"), col("approver"), col("approver_user_id"),
		col("reason"), col("single_operator"), epochUS("decided_at")},
	State:    []string{col("review_status")},
	Optional: true,
}

// PlanEvents chains a plan's history (plan-01b): every plan write and
// engine transition, with the plan hash after it. Append-only. The table
// belongs to the plan objects (internal/changeplan), so the chain installs
// once it exists.
var PlanEvents = Spec{
	Chain: "plan_events", Table: "sage.plan_event", Version: 1,
	Sealed: []string{col("id"), col("plan_id"), col("step_id"), col("kind"),
		col("plan_hash"), col("actor"), col("detail"), epochUS("created_at")},
	Optional: true,
}

// PlanModelRecords chains the model passes a plan was built from
// (plan-01b): the pack, model, prompt version and raw output a later model
// is replayed on. Append-only; every column is sealed.
var PlanModelRecords = Spec{
	Chain: "plan_model_records", Table: "sage.plan_model_record", Version: 1,
	Sealed: []string{col("id"), col("plan_id"), col("pack_hash"), col("model"),
		col("prompt_version"), col("raw_output"), col("raw_output_sha256"),
		epochUS("created_at")},
	Optional: true,
}

// Specs lists the chains the schema migration installs. New chains are
// appended: the order is the install order.
func Specs() []Spec {
	return []Spec{ActionLog, AuthAudit, ConfigAudit, GuardQueryAudit, PGAuditEvents,
		FactEvents, DecisionLedger, PolicyApprovals, PlanEvents, PlanModelRecords}
}

// SpecFor returns the built-in spec of chain.
func SpecFor(chain string) (Spec, bool) {
	for _, s := range Specs() {
		if s.Chain == chain {
			return s, true
		}
	}
	return Spec{}, false
}

// sealedSQL is the canonical text of the sealed columns of row alias.
func (s Spec) sealedSQL(alias string) string {
	return "jsonb_build_array(" + fmt.Sprintf(strings.Join(s.Sealed, ", "), alias) +
		")::text"
}

// sealedHashSQL is the sha256 hex of sealedSQL.
func (s Spec) sealedHashSQL(alias string) string {
	return "encode(sha256(convert_to(" + s.sealedSQL(alias) + ", 'UTF8')), 'hex')"
}

// stateSQL is the canonical text of the state columns, empty when none.
func (s Spec) stateSQL(alias string) string {
	if len(s.State) == 0 {
		return "''"
	}
	return "jsonb_build_array(" + fmt.Sprintf(strings.Join(s.State, ", "), alias) +
		")::text"
}
