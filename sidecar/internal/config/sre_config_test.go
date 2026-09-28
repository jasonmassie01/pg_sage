package config

import (
	"strconv"
	"strings"
	"testing"
)

// Sage SRE M2 config (sre.*): automatic start is opt-in; the intervals
// and retention windows have validated defaults that an absent file, a
// partial section and explicit zeros cannot mask (CHECK-27).

func TestSREDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.SRE
	if s.AutomaticStart || s.TriggerIntervalSeconds != 15 ||
		s.SampleIntervalSeconds != 5 || s.EvidenceRetentionDays != 30 ||
		s.TimelineRetentionDays != 90 {
		t.Fatalf("sre defaults = %+v", s)
	}
}

func TestSREDefaults_PartialSectionKeepsTheRest(t *testing.T) {
	cfg, err := loadRCAYAML(t, "sre:\n  automatic_start: true\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.SRE.AutomaticStart || cfg.SRE.TriggerIntervalSeconds != 15 ||
		cfg.SRE.TimelineRetentionDays != 90 {
		t.Fatalf("sre = %+v, want automatic start with default intervals", cfg.SRE)
	}
}

func TestSREConfig_Boundaries(t *testing.T) {
	cases := []struct {
		key    string
		value  int
		wantOK bool
	}{
		{"trigger_interval_seconds", 0, false}, {"trigger_interval_seconds", 4, false},
		{"trigger_interval_seconds", 5, true}, {"trigger_interval_seconds", 600, true},
		{"trigger_interval_seconds", 601, false},
		{"sample_interval_seconds", 0, false}, {"sample_interval_seconds", 1, true},
		{"sample_interval_seconds", 30, true}, {"sample_interval_seconds", 31, false},
		{"evidence_retention_days", 0, false}, {"evidence_retention_days", -1, false},
		{"evidence_retention_days", 1, true}, {"evidence_retention_days", 90, true},
		{"evidence_retention_days", 91, false}, // longer than the timeline (90)
		{"timeline_retention_days", 29, false}, // shorter than evidence (30)
		{"timeline_retention_days", 30, true}, {"timeline_retention_days", 3650, true},
		{"timeline_retention_days", 3651, false},
	}
	for _, c := range cases {
		t.Run(c.key+"="+strconv.Itoa(c.value), func(t *testing.T) {
			cfg, err := loadRCAYAML(t, "sre:\n  "+c.key+": "+strconv.Itoa(c.value)+"\n")
			if !c.wantOK {
				if err == nil {
					t.Fatalf("%s=%d accepted", c.key, c.value)
				}
				if !strings.Contains(err.Error(), "sre.") {
					t.Fatalf("error must name the sre key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s=%d rejected: %v", c.key, c.value, err)
			}
			if cfg == nil {
				t.Fatal("nil config")
			}
		})
	}
}

func TestSREConfig_UnknownKeyRejected(t *testing.T) {
	if _, err := loadRCAYAML(t, "sre:\n  automatic_strat: true\n"); err == nil {
		t.Fatal("a misspelled sre key was accepted")
	}
}

func TestSREConfig_KeysHaveDocTags(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range []string{"sre.automatic_start", "sre.trigger_interval_seconds",
		"sre.sample_interval_seconds", "sre.evidence_retention_days",
		"sre.timeline_retention_days"} {
		if strings.TrimSpace(docs[key]) == "" {
			t.Errorf("%s has no doc tag", key)
		}
	}
}
