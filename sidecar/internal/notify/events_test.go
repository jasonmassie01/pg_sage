package notify

import "testing"

func TestActionExecutedEvent(t *testing.T) {
	evt := ActionExecutedEvent("Create index", "CREATE INDEX", "mydb")
	if evt.Type != "action_executed" {
		t.Errorf("Type = %q, want action_executed", evt.Type)
	}
	if evt.Severity != "info" {
		t.Errorf("Severity = %q, want info", evt.Severity)
	}
	if evt.Data["database"] != "mydb" {
		t.Errorf("Data[database] = %v, want mydb", evt.Data["database"])
	}
}

func TestActionFailedEvent(t *testing.T) {
	evt := ActionFailedEvent("Drop index", "DROP INDEX", "mydb", "timeout")
	if evt.Type != "action_failed" {
		t.Errorf("Type = %q, want action_failed", evt.Type)
	}
	if evt.Severity != "warning" {
		t.Errorf("Severity = %q, want warning", evt.Severity)
	}
	if evt.Data["error"] != "timeout" {
		t.Errorf("Data[error] = %v, want timeout", evt.Data["error"])
	}
}

func TestApprovalNeededEvent(t *testing.T) {
	evt := ApprovalNeededEvent("Reindex", "REINDEX", "mydb", "high")
	if evt.Type != "approval_needed" {
		t.Errorf("Type = %q, want approval_needed", evt.Type)
	}
	if evt.Data["risk"] != "high" {
		t.Errorf("Data[risk] = %v, want high", evt.Data["risk"])
	}
}

func TestFindingCriticalEvent(t *testing.T) {
	evt := FindingCriticalEvent("Disk full", "90% used", "prod")
	if evt.Type != "finding_critical" {
		t.Errorf("Type = %q, want finding_critical", evt.Type)
	}
	if evt.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", evt.Severity)
	}
}
