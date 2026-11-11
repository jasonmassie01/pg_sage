package siem

import "fmt"

// decisionMapping: the policy gate decided (I), or a decision's outcome
// changed later (U: queued, resolved, turned into a block) (audit-11). An
// execute at tier high ran without a person's approval of that change,
// so it is raised to medium.
func decisionMapping(r Record) mapping {
	verdict, tier := str(r.Row, "verdict"), str(r.Row, "risk_tier")
	m := mapping{class: ClassAPIActivity, activity: 1, activityName: "Create",
		status: statusSuccess, severity: SeverityInformational,
		message: fmt.Sprintf("pg_sage decision %d: %s at tier %s (%s %s)", r.Link.RowID,
			verdict, tier, str(r.Row, "feature"), str(r.Row, "intent")),
		extra: map[string]any{"database": databaseObject(r.Source)}}
	if r.Link.Op == "U" {
		m.activity, m.activityName = 3, "Update"
		m.message = fmt.Sprintf("pg_sage decision %d is now %s at tier %s",
			r.Link.RowID, verdict, tier)
		return m
	}
	if verdict == "execute" && tier == "high" {
		m.severity = SeverityMedium
	}
	return m
}

// approvalMapping: a person approved a policy version (I), or a
// single-operator approval was reviewed (U). A single-operator approval
// still awaiting its review is raised to medium.
func approvalMapping(r Record) mapping {
	single := r.Row != nil && r.Row["single_operator"] == true
	kind := "approval"
	if single {
		kind = "single-operator approval"
	}
	m := mapping{class: ClassEntityManagement, activity: 1, activityName: "Create",
		status: statusSuccess, severity: SeverityInformational,
		message: fmt.Sprintf("pg_sage policy %s %d: policy %s by %s", kind, r.Link.RowID,
			str(r.Row, "policy_id"), str(r.Row, "approver"))}
	if r.Link.Op == "U" {
		m.activity, m.activityName = 3, "Update"
		m.message = fmt.Sprintf("pg_sage policy %s %d of policy %s is now %s", kind,
			r.Link.RowID, str(r.Row, "policy_id"), str(r.Row, "review_status"))
		return m
	}
	if single && str(r.Row, "review_status") == "pending" {
		m.severity = SeverityMedium
	}
	return m
}
