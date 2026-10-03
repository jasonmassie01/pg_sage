package executor

import "testing"

func TestIsManagedProvider(t *testing.T) {
	managed := []string{"rds", "aurora", "cloud-sql", "cloudsql", "alloydb", "azure", "AWS"}
	for _, p := range managed {
		if !isManagedProvider(p) {
			t.Errorf("isManagedProvider(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"self-managed", "postgres", "", "ec2"} {
		if isManagedProvider(p) {
			t.Errorf("isManagedProvider(%q) = true, want false", p)
		}
	}
}

func TestOutcomeStatus(t *testing.T) {
	if outcomeStatus(true) != "success" {
		t.Error("in-effect should be success")
	}
	if outcomeStatus(false) != "applied_pending_restart" {
		t.Error("not-in-effect should be applied_pending_restart")
	}
}
