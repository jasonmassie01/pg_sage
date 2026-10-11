package config

import (
	"strings"
	"testing"
)

// retention.auth_audit_days: the SSO and break-glass audit trail
// (sage.auth_audit) ages out after this many days. Default 365; 0 keeps it
// forever; out-of-range is refused.

func TestAuthAuditDays_DefaultWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retention.AuthAuditDays != 365 || DefaultRetentionAuthAuditDays != 365 {
		t.Fatalf("default = %d (const %d), want 365", cfg.Retention.AuthAuditDays,
			DefaultRetentionAuthAuditDays)
	}
	if DefaultConfig().Retention.AuthAuditDays != 365 {
		t.Fatal("DefaultConfig disagrees with Load(nil)")
	}
}

func TestAuthAuditDays_PartialSectionKeepsDefault(t *testing.T) {
	cfg, err := loadRCAYAML(t, "retention:\n  actions_days: 400\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retention.AuthAuditDays != 365 || cfg.Retention.ActionsDays != 400 {
		t.Fatalf("retention = %+v, want the default auth audit window kept", cfg.Retention)
	}
}

func TestAuthAuditDays_ExplicitValues(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want int
	}{{"0", 0}, {"1", 1}, {"90", 90}, {"3650", 3650}} {
		cfg, err := loadRCAYAML(t, "retention:\n  auth_audit_days: "+tc.yaml+"\n")
		if err != nil || cfg.Retention.AuthAuditDays != tc.want {
			t.Errorf("%s: got %d (%v), want %d", tc.yaml, cfg.Retention.AuthAuditDays, err,
				tc.want)
		}
	}
}

func TestAuthAuditDays_OutOfRangeRefused(t *testing.T) {
	for _, v := range []string{"-1", "3651"} {
		_, err := loadRCAYAML(t, "retention:\n  auth_audit_days: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "retention.auth_audit_days") {
			t.Errorf("%s: err = %v, want a refusal naming the key", v, err)
		}
	}
}
