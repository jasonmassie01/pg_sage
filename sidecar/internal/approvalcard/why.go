package approvalcard

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/pgconf"
)

// whyOf says why the action waits for a person, most specific first: the
// producer's marker, a configuration change pg_sage never makes alone, an
// unverified index, the gate's decision, an earlier operator rejection of
// the same change, and the trust level. With none of those it still says
// the policy queued it. A missing finding is called out on its own.
func whyOf(in Inputs, actionType string) []Reason {
	var out []Reason
	if in.Finding != nil {
		out = append(out, producerReasons(in.Finding.Detail)...)
	}
	out = append(out, gucReasons(in.Action.ProposedSQL)...)
	if r, ok := whatIfReason(in, actionType); ok {
		out = append(out, r)
	}
	if d := in.Decision; d != nil && d.Reason != "" && d.Reason != "authorized" {
		out = append(out, Reason{Code: "gate:" + d.Reason, Text: fmt.Sprintf(
			"Policy gate: %s (%s risk)", humanize(d.Reason), orUnknown(d.RiskTier))})
	}
	if r := in.Rejection; r != nil {
		text := fmt.Sprintf("An operator rejected this exact change on %s (queue item %d)",
			r.DecidedAt.UTC().Format("2006-01-02 15:04 UTC"), r.QueueID)
		if strings.TrimSpace(r.Reason) != "" {
			text += ": " + r.Reason
		}
		out = append(out, Reason{Code: "operator_rejected_before", Text: text})
	}
	if level := strings.TrimSpace(in.TrustLevel); level == "observation" ||
		level == "advisory" {
		out = append(out, Reason{Code: "trust_level", Text: fmt.Sprintf(
			"Trust level is %s: pg_sage proposes changes and a person approves them", level)})
	}
	if len(out) == 0 {
		decision := in.Action.PolicyDecision
		if decision == "" {
			decision = "queue_approval"
		}
		out = append(out, Reason{Code: "queued_for_approval", Text: fmt.Sprintf(
			"The policy queued this change for approval (%s)", humanize(decision))})
	}
	if in.Finding == nil {
		out = append(out, Reason{Code: "finding_missing", Text: "The finding behind this " +
			"action is gone: check that the change still applies"})
	}
	return out
}

// producerReasons is the producer's approval_required marker.
func producerReasons(detail map[string]any) []Reason {
	switch v := detail["approval_required"].(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			return []Reason{{Code: "approval_required", Text: v}}
		}
	case bool:
		if v {
			return []Reason{{Code: "approval_required",
				Text: "The finding is marked approval required"}}
		}
	}
	return nil
}

// gucReasons: a server setting that needs a restart or is not on the
// autonomous allowlist is always a person's decision.
func gucReasons(sql string) []Reason {
	stmt, ok := pgconf.ParseAlterSystem(sql)
	if !ok {
		return nil
	}
	var out []Reason
	if pgconf.RequiresRestart(stmt.Name) {
		out = append(out, Reason{Code: "guc_restart", Text: stmt.Name +
			" takes effect only after a restart"})
	}
	if !pgconf.AutonomousGUC(stmt.Name) {
		out = append(out, Reason{Code: "guc_not_allowlisted", Text: stmt.Name +
			" is not on the autonomous configuration allowlist"})
	}
	return out
}

// whatIfReason: an index HypoPG has not verified needs approval.
func whatIfReason(in Inputs, actionType string) (Reason, bool) {
	if in.Finding == nil || actionType != "create_index_concurrently" {
		return Reason{}, false
	}
	verdict, _ := in.Finding.Detail["what_if_verdict"].(string)
	if verdict == "verified" {
		return Reason{}, false
	}
	if verdict == "" {
		verdict = "missing"
	}
	text := "HypoPG what-if is " + verdict + ": the improvement is not verified"
	if why, _ := in.Finding.Detail["what_if_reason"].(string); why != "" {
		text += " (" + why + ")"
	}
	return Reason{Code: "what_if_unverified", Text: text}, true
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}
