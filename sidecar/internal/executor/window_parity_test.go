package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// D2 T2: trust.maintenance_window and policy windows are one grammar. Every
// row of the memo's divergence table must give the same answer through the
// config path (inMaintenanceWindowAt) and the policy path (ParseWindow).
func TestConfigAndPolicyWindowsAgree(t *testing.T) {
	at := func(day, hour, minute int) time.Time { // 2026-09-26 is a Saturday
		return time.Date(2026, 9, day, hour, minute, 0, 0, time.UTC)
	}
	cases := []struct {
		expr string
		at   time.Time
		want bool
	}{
		{"0 2 * * *", at(28, 2, 30), true},
		{"0 2 * * 0", at(27, 2, 30), true},
		{"30 * * * *", at(28, 5, 45), true},
		{"30 * * * *", at(28, 5, 10), true},
		{"0 2 * * 1-5", at(28, 2, 0), true},
		{"*/15 2 * * *", at(28, 2, 15), true},
		{"weekdays 22:00-06:00", at(26, 2, 0), true},
		{"weekdays 22:00-06:00", at(28, 2, 0), false},
		{"weeknights", at(26, 2, 0), true},
		{"weeknights", at(28, 2, 0), false},
		{"nights", at(28, 23, 0), true},
		{"weekdays", at(28, 12, 0), true},
		{"daily 01:00-05:00", at(28, 2, 0), true},
		{"Mon-Fri 01:00-05:00", at(28, 2, 0), true},
		{"Mon-Fri 01:00-05:00", at(26, 2, 0), false},
		{"sat,sun 02:00-06:00", at(26, 3, 0), true},
		{"22:00-02:00", at(26, 23, 0), true},
		{"always", at(28, 12, 0), true},
		{"weekends", at(26, 12, 0), true},
		{"weekdays 01:00-05:00", at(28, 2, 0), true},
		{"weekdays 01:00-05:00 America/New_York", at(28, 6, 0), true},
	}
	for _, c := range cases {
		window, err := policy.ParseWindow(c.expr)
		if err != nil {
			t.Errorf("policy.ParseWindow(%q): %v", c.expr, err)
			continue
		}
		policyResult := window.Contains(c.at)
		configResult := inMaintenanceWindowAt(c.expr, c.at)
		if policyResult != c.want || configResult != c.want {
			t.Errorf("%q at %s: policy=%v config=%v, want both %v", c.expr,
				c.at.Format("Mon 15:04"), policyResult, configResult, c.want)
		}
	}
}

// Config-only values: unset and "never" close the window; an invalid value
// (possible only for a row stored before validation existed) fails closed.
func TestConfigOnlyWindowValuesAreClosed(t *testing.T) {
	now := time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC)
	for _, expr := range []string{"", "   ", "never", "off", "none", "disabled",
		"garbage", "00:00-00:00"} {
		if inMaintenanceWindowAt(expr, now) {
			t.Errorf("inMaintenanceWindowAt(%q) = true, want false", expr)
		}
	}
}

// D2 T8: an autonomous moderate action with trust window "nights" and a
// policy window "always" executes at 23:00.
func TestGateNightsTrustWindowWithAlwaysPolicy(t *testing.T) {
	now := time.Date(2026, 9, 28, 23, 0, 0, 0, time.UTC)
	cfg := &config.Config{Trust: config.TrustConfig{
		Level: "autonomous", Tier3Safe: true, Tier3Moderate: true,
		MaintenanceWindow: "nights",
	}}
	e := New(nil, cfg, now.Add(-40*24*time.Hour), noopExecLog)
	e.emergencyStopFn = func(context.Context) bool { return false }
	e.SetExecutionMode("auto")
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	e.EnableStandingPolicyDocument(doc, func() time.Time { return now })

	got := e.ExplainFamilies(context.Background(),
		[]ActionContract{riskContract("moderate")}, false)[0]

	if got.Decision != PolicyDecisionExecute {
		t.Fatalf("decision = %+v, want execute", got)
	}
}
