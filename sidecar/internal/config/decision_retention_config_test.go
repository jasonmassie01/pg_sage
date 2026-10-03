package config

import (
	"strings"
	"testing"
)

// retention.decisions_days: non-execute decisions (withheld gate verdicts,
// schema guard observations) age out after this many days of not being
// seen again. Default 30; 0 disables the rule; out-of-range is refused.

func TestDecisionsDays_DefaultWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retention.DecisionsDays != 30 || DefaultRetentionDecisionsDays != 30 {
		t.Fatalf("default = %d (const %d), want 30", cfg.Retention.DecisionsDays,
			DefaultRetentionDecisionsDays)
	}
	if DefaultConfig().Retention.DecisionsDays != 30 {
		t.Fatal("DefaultConfig disagrees with Load(nil)")
	}
}

func TestDecisionsDays_PartialSectionKeepsDefault(t *testing.T) {
	cfg, err := loadRCAYAML(t, "retention:\n  actions_days: 400\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retention.DecisionsDays != 30 || cfg.Retention.ActionsDays != 400 {
		t.Fatalf("retention = %+v, want the default decisions window kept", cfg.Retention)
	}
}

func TestDecisionsDays_ExplicitValues(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want int
	}{{"0", 0}, {"1", 1}, {"90", 90}, {"3650", 3650}} {
		cfg, err := loadRCAYAML(t, "retention:\n  decisions_days: "+tc.yaml+"\n")
		if err != nil || cfg.Retention.DecisionsDays != tc.want {
			t.Errorf("%s: got %+v (%v), want %d", tc.yaml, cfg, err, tc.want)
		}
	}
}

func TestDecisionsDays_OutOfRangeRefused(t *testing.T) {
	for _, v := range []string{"-1", "3651"} {
		_, err := loadRCAYAML(t, "retention:\n  decisions_days: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "retention.decisions_days") {
			t.Errorf("%s: err = %v, want a refusal naming the key", v, err)
		}
	}
}
