package notify

import "fmt"

// CustodianEscalation is a custodian proposal that exhausted its failure
// backoff (vacuum-05): pg_sage stops retrying it, so an operator must act.
type CustodianEscalation struct {
	Database   string
	Feature    string
	Target     string
	SQL        string
	SQLSHA256  string
	Failures   int
	Window     string
	DecisionID int64
	// Profile names a non-default VACUUM profile (vacuum-05b: "emergency");
	// empty for every other proposal.
	Profile string
}

// CustodianEscalationEvent is the one critical event of an exhausted
// backoff. Its dedup key (custodian_backoff:<db>:<sql sha256>) makes a
// repeat a PagerDuty no-op.
func CustodianEscalationEvent(info CustodianEscalation) Event {
	decision := "none"
	if info.DecisionID > 0 {
		decision = fmt.Sprintf("%d", info.DecisionID)
	}
	evt := Event{
		Type:     "custodian_escalation",
		Severity: "critical",
		Subject: fmt.Sprintf("Custodian gave up on %s: %d failures in %s",
			info.Target, info.Failures, info.Window),
		Body: fmt.Sprintf("Database: %s\nFeature: %s\nSQL: %s\nRecorded as decision %s.\n"+
			"pg_sage will not retry it until its failures age out of the window: find "+
			"why it fails (lock_timeout behind autovacuum, the watchdog's vacuum_cap or "+
			"vacuum_stalled, privileges) in its action log and act.",
			info.Database, info.Feature, info.SQL, decision),
		Data: map[string]any{
			"database": info.Database, "feature": info.Feature, "target": info.Target,
			"sql": info.SQL, "failures": info.Failures, "window": info.Window,
			"decision_id": info.DecisionID,
		},
		DedupKey: fmt.Sprintf("custodian_backoff:%s:%s", info.Database, info.SQLSHA256),
	}
	if info.Profile != "" {
		evt.Body = "Profile: " + info.Profile + "\n" + evt.Body
		evt.Data["profile"] = info.Profile
	}
	return evt
}
