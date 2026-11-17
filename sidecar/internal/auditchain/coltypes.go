package auditchain

import "regexp"

// SpecKey names one payload version of one chain.
type SpecKey struct {
	Chain   string
	Version int
}

const tstz = "timestamp with time zone"

// SealedColumnTypes pins, per chain and payload version, the type
// (format_type) of every column the spec's Sealed and State expressions
// read. jsonb_build_array renders a value by its type, so changing one of
// these types rewrites the canonical text of every historical row, and a
// clean trail would verify as edited. Change such a type only together
// with a new Spec.Version and a new entry here, and keep the old entry for
// the links written under it (schema's audit chain types test enforces it).
var SealedColumnTypes = map[SpecKey]map[string]string{
	{Chain: "action_log", Version: 1}: {
		"id": "bigint", "executed_at": tstz, "action_type": "text", "finding_id": "bigint",
		"sql_executed": "text", "rollback_sql": "text", "before_state": "jsonb",
		"approved_by": "integer", "approved_at": tstz, "decision_id": "bigint",
		"database_id": "bigint", "outcome": "text", "rollback_reason": "text",
		"measured_at": tstz, "after_state": "jsonb", "verification_id": "bigint",
		"justification": "text", "toil_minutes_saved": "numeric",
		"toil_model_version": "integer",
	},
	{Chain: "auth_audit", Version: 1}: {
		"id": "bigint", "event": "text", "actor_user_id": "integer",
		"target_user_id": "integer", "detail": "jsonb", "created_at": tstz,
		"source_ip": "text",
	},
	{Chain: "config_audit", Version: 1}: {
		"id": "integer", "key": "text", "old_value": "text", "new_value": "text",
		"database_id": "integer", "changed_by": "integer", "changed_at": tstz,
		"changed_by_actor": "text",
	},
	{Chain: "guard_query_audit", Version: 1}: {
		"id": "bigint", "database_id": "uuid", "principal_id": "text", "task_id": "text",
		"envelope_hash": "text", "verdict": "text", "reason": "text",
		"row_count": "integer", "classes": "text[]", "at": tstz,
	},
	{Chain: "pgaudit_events", Version: 1}: {
		"id": "bigint", "database_name": "text", "logged_at": tstz, "session_id": "text",
		"pid": "integer", "db_user": "text", "application": "text",
		"principal_id": "text", "action_id": "bigint", "audit_type": "text",
		"statement_id": "bigint", "substatement_id": "bigint", "class": "text",
		"command": "text", "object_type": "text", "object_name": "text",
		"statement": "text", "correlated_by": "text",
	},
	// facts-07's fact history chain.
	{Chain: "fact_events", Version: 1}: {
		"id": "bigint", "fact_id": "bigint", "at": tstz, "actor": "text", "verb": "text",
		"from_status": "text", "to_status": "text", "fact_hash": "text", "note": "text",
	},
	// audit-11's decision ledger and policy approval chains.
	{Chain: "decision", Version: 1}: {
		"id": "bigint", "database_id": "bigint", "feature": "text", "intent": "text",
		"target_objects": "jsonb", "policy_version": "integer", "policy_doc_sha256": "text",
		"risk_tier": "text", "evidence_id": "text", "fingerprint": "text",
		"principal_id": "text", "task_id": "text", "artifact_hash": "text",
		"created_at": tstz, "verdict": "text", "reason": "text", "evidence": "jsonb",
		"guardrails": "jsonb", "off_window_ok": "boolean", "deadline_kind": "text",
		"deadline_hard_at": tstz, "queue_id": "integer", "action_log_id": "bigint",
		"resolved_at": tstz, "candidate_key": "text",
	},
	{Chain: "policy_approvals", Version: 1}: {
		"id": "bigint", "policy_id": "bigint", "approver": "text",
		"approver_user_id": "integer", "reason": "text", "single_operator": "boolean",
		"decided_at": tstz, "review_status": "text",
	},
	// plan-01b's plan event and model record chains.
	{Chain: "plan_events", Version: 1}: {
		"id": "bigint", "plan_id": "bigint", "step_id": "bigint", "kind": "text",
		"plan_hash": "text", "actor": "text", "detail": "jsonb", "created_at": tstz,
	},
	{Chain: "plan_model_records", Version: 1}: {
		"id": "bigint", "plan_id": "bigint", "pack_hash": "text", "model": "text",
		"prompt_version": "text", "raw_output": "text", "raw_output_sha256": "text",
		"created_at": tstz,
	},
}

var columnRef = regexp.MustCompile(`%\[1\]s\.([a-z_][a-z0-9_]*)`)

// Columns returns the table columns the Sealed and State expressions read,
// in expression order, each once.
func (s Spec) Columns() []string {
	var out []string
	seen := map[string]bool{}
	for _, expr := range append(append([]string(nil), s.Sealed...), s.State...) {
		for _, m := range columnRef.FindAllStringSubmatch(expr, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	return out
}
