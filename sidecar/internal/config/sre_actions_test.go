package config

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Sage SRE M5 action config (sre.actions.*): proposals are on by default
// (the AI-DBA principle), execution always needs a human approval, the
// evidence age can only be tightened below 5 s, and defaults survive an
// absent file, a partial section and other sre keys (CHECK-27).

func TestSREActionsDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.SRE.Actions
	if !a.Proposals || !a.RequestApproval || a.ApprovalTTLMinutes != 15 ||
		a.MaxEvidenceAgeSeconds != 5 || a.RecoverySampleSeconds != 40 ||
		a.RecoverySamples != 3 || a.RecoveryDeadlineMinutes != 30 ||
		a.ChatOpsToleranceSeconds != 300 || len(a.ProtectedRoles) != 0 ||
		len(a.ProtectedApplications) != 0 {
		t.Fatalf("sre.actions defaults = %+v", a)
	}
	if a.ApprovalTTL() != 15*time.Minute || a.MaxEvidenceAge() != 5*time.Second ||
		a.RecoveryInterval() != 40*time.Second || a.RecoveryDeadline() != 30*time.Minute ||
		a.ChatOpsTolerance() != 5*time.Minute {
		t.Fatalf("durations = %s %s %s %s %s", a.ApprovalTTL(), a.MaxEvidenceAge(),
			a.RecoveryInterval(), a.RecoveryDeadline(), a.ChatOpsTolerance())
	}
}

func TestSREActionsDefaults_OtherSREKeysKeepThem(t *testing.T) {
	cfg, err := loadRCAYAML(t, "sre:\n  automatic_start: true\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.SRE.Actions.Proposals || cfg.SRE.Actions.MaxEvidenceAgeSeconds != 5 {
		t.Fatalf("sre.actions = %+v, want defaults", cfg.SRE.Actions)
	}
	cfg, err = loadRCAYAML(t, "sre:\n  actions:\n    recovery_samples: 5\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SRE.Actions.RecoverySamples != 5 || !cfg.SRE.Actions.Proposals ||
		cfg.SRE.Actions.ApprovalTTLMinutes != 15 {
		t.Fatalf("partial sre.actions = %+v", cfg.SRE.Actions)
	}
}

func TestSREActionsOptOutAndProtectedBackends(t *testing.T) {
	cfg, err := loadRCAYAML(t, "sre:\n  actions:\n    proposals: false\n"+
		"    request_approval: false\n    protected_roles: [replicator, dba]\n"+
		"    protected_applications: [billing-batch]\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.SRE.Actions
	if a.Proposals || a.RequestApproval || len(a.ProtectedRoles) != 2 ||
		a.ProtectedRoles[1] != "dba" || a.ProtectedApplications[0] != "billing-batch" {
		t.Fatalf("sre.actions = %+v", a)
	}
}

func TestSREActionsBoundaries(t *testing.T) {
	cases := []struct {
		key    string
		value  int
		wantOK bool
	}{
		{"approval_ttl_minutes", 0, false}, {"approval_ttl_minutes", 1, true},
		{"approval_ttl_minutes", 60, true}, {"approval_ttl_minutes", 61, false},
		{"max_evidence_age_seconds", 0, false}, {"max_evidence_age_seconds", 1, true},
		{"max_evidence_age_seconds", 5, true}, {"max_evidence_age_seconds", 6, false},
		{"recovery_sample_seconds", 0, false}, {"recovery_sample_seconds", 1, true},
		{"recovery_sample_seconds", 300, true}, {"recovery_sample_seconds", 301, false},
		{"recovery_samples", 2, false}, {"recovery_samples", 3, true},
		{"recovery_samples", 20, true}, {"recovery_samples", 21, false},
		{"recovery_deadline_minutes", 0, false}, {"recovery_deadline_minutes", 2, true},
		{"recovery_deadline_minutes", 240, true}, {"recovery_deadline_minutes", 241, false},
		{"chatops_tolerance_seconds", 29, false}, {"chatops_tolerance_seconds", 30, true},
		{"chatops_tolerance_seconds", 900, true}, {"chatops_tolerance_seconds", 901, false},
	}
	for _, c := range cases {
		t.Run(c.key+"="+strconv.Itoa(c.value), func(t *testing.T) {
			_, err := loadRCAYAML(t, "sre:\n  actions:\n    "+c.key+": "+
				strconv.Itoa(c.value)+"\n")
			if c.wantOK && err != nil {
				t.Fatalf("%s=%d rejected: %v", c.key, c.value, err)
			}
			if !c.wantOK && (err == nil || !strings.Contains(err.Error(), "sre.actions.")) {
				t.Fatalf("%s=%d = %v, want an error naming sre.actions", c.key, c.value, err)
			}
		})
	}
}

// The recovery observation must fit before its deadline.
func TestSREActionsRecoveryMustFitTheDeadline(t *testing.T) {
	_, err := loadRCAYAML(t, "sre:\n  actions:\n    recovery_sample_seconds: 300\n"+
		"    recovery_samples: 20\n    recovery_deadline_minutes: 60\n")
	if err == nil || !strings.Contains(err.Error(), "recovery_deadline_minutes") {
		t.Fatalf("20 samples of 300 s in 60 min = %v, want a deadline error", err)
	}
}

func TestSREActionsRejectsBlankProtectedNames(t *testing.T) {
	for _, yaml := range []string{
		"sre:\n  actions:\n    protected_roles: [\"\"]\n",
		"sre:\n  actions:\n    protected_applications: [\"  \"]\n",
	} {
		if _, err := loadRCAYAML(t, yaml); err == nil {
			t.Fatalf("blank protected name accepted: %q", yaml)
		}
	}
}

func TestSREActionsUnknownKeyRejected(t *testing.T) {
	if _, err := loadRCAYAML(t, "sre:\n  actions:\n    auto_execute: true\n"); err == nil {
		t.Fatal("an unknown sre.actions key (auto_execute) was accepted")
	}
}
