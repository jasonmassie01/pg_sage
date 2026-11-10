package evidence

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// audit-07: window packs left out the chained sage.guard_query_audit, and
// action packs the approvals of the policy their decision names.

func TestWindowPackHoldsGuardQueryAudit(t *testing.T) {
	pool := freshDB(t, "evidence_guard_query")
	mustExec(t, pool, `INSERT INTO sage.guard_query_audit (database_id, principal_id,
		envelope_hash, verdict) VALUES (gen_random_uuid(), 'agent-7',
		'envelope-hash-in-window', 'execute'), (gen_random_uuid(), 'agent-7',
		'envelope-hash-too-old', 'blocked')`)
	mustExec(t, pool, `UPDATE sage.guard_query_audit SET at = now() - interval '9 days'
		WHERE envelope_hash = 'envelope-hash-too-old'`)
	m, files := buildFrom(t, Sources{DB: pool}, lastHour())
	guard := string(files["guard_query_audit.json"])
	if !strings.Contains(guard, "envelope-hash-in-window") ||
		strings.Contains(guard, "envelope-hash-too-old") {
		t.Fatalf("guard_query_audit.json = %s, want the window's row only", guard)
	}
	if e := entry(t, m, "guard_query_audit.json"); e.Rows == nil || *e.Rows != 1 ||
		e.Source != "database" {
		t.Fatalf("guard_query_audit.json entry = %+v", e)
	}
	p := proofOf(t, files, "guard_query_audit")
	if p.Verification == nil || p.Verification.Links < 1 || !m.ChainOK {
		t.Fatalf("guard_query_audit proof = %+v chain_ok %v", p, m.ChainOK)
	}
	mustExec(t, pool, `UPDATE sage.guard_query_audit SET verdict = 'blocked'
		WHERE envelope_hash = 'envelope-hash-in-window'`)
	m, files = buildFrom(t, Sources{DB: pool}, lastHour())
	if p := proofOf(t, files, "guard_query_audit"); m.ChainOK || p.Verification == nil ||
		len(p.Verification.Problems) == 0 {
		t.Fatalf("an edited guard query row was not reported: %+v", p)
	}
}

// Fleet: the approvals of the policy the decision names come from the
// control database; another policy's approval is not in the pack.
func TestActionPackHoldsPolicyApprovals(t *testing.T) {
	monitored := freshDB(t, "evidence_approvals_m")
	control := freshDB(t, "evidence_approvals_c")
	hash := seedPolicy(t, control)
	action := seedExecutedDecision(t, monitored, &hash)
	mustExec(t, control, `INSERT INTO sage.policy (database_id, version, profile, doc,
		status, proposed_by, ratified_by) VALUES (41, 4, 'unattended', '{}'::jsonb,
		'proposed', 'test', 'test')`)
	mustExec(t, control, `INSERT INTO sage.policy_approvals (policy_id, approver,
		approver_user_id, reason) SELECT id, 'ana@example.com', 3, 'reviewed v3'
		FROM sage.policy WHERE version = 3`)
	mustExec(t, control, `INSERT INTO sage.policy_approvals (policy_id, approver,
		approver_user_id, reason) SELECT id, 'decoy@example.com', 4, 'other version'
		FROM sage.policy WHERE version = 4`)
	pack, err := Build(context.Background(), Sources{DB: monitored, Control: control},
		Scope{ActionID: action}, nil, time.Now())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var raw []byte
	for _, f := range pack.Files {
		if f.Name == "policy_approvals.json" {
			raw = f.Data
		}
	}
	var approvals []struct {
		Approver string `json:"approver"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &approvals); err != nil {
		t.Fatalf("policy_approvals.json %q: %v", raw, err)
	}
	if len(approvals) != 1 || approvals[0].Approver != "ana@example.com" ||
		approvals[0].Reason != "reviewed v3" {
		t.Fatalf("policy_approvals.json = %+v, want v3's approval only", approvals)
	}
	if e := entry(t, pack.Manifest, "policy_approvals.json"); e.Source != "control" ||
		e.Rows == nil || *e.Rows != 1 || e.Truncated {
		t.Fatalf("policy_approvals.json entry = %+v", e)
	}
	if e := entry(t, pack.Manifest, "policy.json"); e.Source != "control" {
		t.Fatalf("policy.json source = %q, want control", e.Source)
	}
}

// A decision naming no policy gives an empty policy_approvals.json.
func TestActionPackWithoutPolicyHasNoApprovals(t *testing.T) {
	pool := freshDB(t, "evidence_approvals_none")
	action := seedExecutedDecision(t, pool, nil)
	mustExec(t, pool, `UPDATE sage.decision SET policy_version = 0`)
	m, files := buildFrom(t, Sources{DB: pool}, Scope{ActionID: action})
	if strings.TrimSpace(string(files["policy_approvals.json"])) != "[]" {
		t.Fatalf("policy_approvals.json = %s, want []", files["policy_approvals.json"])
	}
	if e := entry(t, m, "policy_approvals.json"); e.Rows == nil || *e.Rows != 0 ||
		e.Source != "database" {
		t.Fatalf("policy_approvals.json entry = %+v", e)
	}
}
