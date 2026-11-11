package siem

import (
	"strings"
	"testing"
)

// No concurrent access tests: mapping is a pure function of one record.

// audit-11: decision and approval links fell through to the "Other"
// fallback. A decision names its verdict and tier, and an autonomous
// execute at tier high is raised to medium; an approval names the
// approver and the policy, and a single-operator approval awaiting review
// is raised to medium.
func TestDecisionMapping(t *testing.T) {
	cases := []struct {
		op       string
		row      map[string]any
		severity int
		words    []string
	}{
		{"I", map[string]any{"verdict": "execute", "risk_tier": "high", "feature": "index",
			"intent": "create_index"}, SeverityMedium,
			[]string{"decision 7", "execute", "high", "index"}},
		{"I", map[string]any{"verdict": "queue_approval", "risk_tier": "high"},
			SeverityInformational, []string{"queue_approval", "high"}},
		{"I", map[string]any{"verdict": "execute", "risk_tier": "safe"},
			SeverityInformational, []string{"execute", "safe"}},
		{"U", map[string]any{"verdict": "blocked", "risk_tier": "moderate"},
			SeverityInformational, []string{"decision 7", "blocked"}},
		{"I", nil, SeverityInformational, []string{"decision 7"}},
	}
	for _, c := range cases {
		e := Map(Record{Source: "orders", Chain: "decision", Link: link("decision", 1, c.op),
			Row: c.row})
		assertMapped(t, e, c.row, c.severity, c.words)
	}
}

func TestApprovalMapping(t *testing.T) {
	cases := []struct {
		op       string
		row      map[string]any
		severity int
		words    []string
	}{
		{"I", map[string]any{"policy_id": 12, "approver": "alice@example.com",
			"single_operator": true, "review_status": "pending"}, SeverityMedium,
			[]string{"policy 12", "alice@example.com", "single-operator"}},
		{"I", map[string]any{"policy_id": 12, "approver": "bob", "single_operator": false},
			SeverityInformational, []string{"policy 12", "bob"}},
		{"U", map[string]any{"policy_id": 12, "approver": "alice", "single_operator": true,
			"review_status": "reviewed"}, SeverityInformational, []string{"reviewed"}},
		{"I", nil, SeverityInformational, []string{"approval"}},
	}
	for _, c := range cases {
		e := Map(Record{Source: "control", Chain: "policy_approvals",
			Link: link("policy_approvals", 1, c.op), Row: c.row})
		assertMapped(t, e, c.row, c.severity, c.words)
	}
}

func assertMapped(t *testing.T, e Event, row map[string]any, severity int, words []string) {
	t.Helper()
	msg, _ := e["message"].(string)
	if e["activity_name"] == "Other" || strings.HasPrefix(msg, "pg_sage audit event") ||
		num(t, e, "severity_id") != severity || num(t, e, "class_uid") == ClassAPIActivity &&
		num(t, e, "activity_id") == 99 {
		t.Fatalf("row %v: %v / %q / severity %v, want a mapped event at %d", row,
			e["activity_name"], msg, e["severity_id"], severity)
	}
	for _, w := range words {
		if !strings.Contains(msg, w) {
			t.Fatalf("row %v: message %q lacks %q", row, msg, w)
		}
	}
}

// pg_sage's own retention delete of a decision is routine; a delete by
// anyone else is worth a look, in the decision's own class.
func TestDecisionTrailRemoval(t *testing.T) {
	own := Map(Record{Source: "orders", Chain: "decision", Link: link("decision", 2, "D"),
		OwnRole: "sage"})
	foreign := link("decision", 3, "D")
	foreign.DBUser = "mallory"
	other := Map(Record{Source: "orders", Chain: "decision", Link: foreign, OwnRole: "sage"})
	if num(t, own, "severity_id") != SeverityInformational ||
		num(t, other, "severity_id") != SeverityMedium ||
		num(t, own, "class_uid") != num(t, Map(Record{Chain: "decision",
			Link: link("decision", 1, "I")}), "class_uid") {
		t.Fatalf("decision deletes: own %v, other %v", own, other)
	}
}
