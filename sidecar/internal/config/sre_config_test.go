package config

import (
	"strconv"
	"strings"
	"testing"
)

// Sage SRE config (sre.*): automatic start is on by default since M4
// (investigations are read-only and bounded); the intervals and
// retention windows have validated defaults that an absent file, a
// partial section and explicit zeros cannot mask (CHECK-27).

func TestSREDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.SRE
	if !s.AutomaticStart || s.TriggerIntervalSeconds != 15 ||
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

// Sage SRE M3: the model turn (sre.llm.enabled) is on by default; it is
// used only when an LLM is configured, and false turns it off.
func TestSRELLM_DefaultsOn(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.SRE.LLM.Enabled || !DefaultConfig().SRE.LLM.Enabled {
		t.Fatalf("sre.llm.enabled default = %v, want true", cfg.SRE.LLM.Enabled)
	}
	partial, err := loadRCAYAML(t, "sre:\n  automatic_start: true\n")
	if err != nil || !partial.SRE.LLM.Enabled {
		t.Fatalf("a partial sre section turned the model off: %+v (%v)", partial, err)
	}
	empty, err := loadRCAYAML(t, "sre:\n  llm: {}\n")
	if err != nil || !empty.SRE.LLM.Enabled {
		t.Fatalf("an empty sre.llm section turned the model off: %+v (%v)", empty, err)
	}
}

func TestSRELLM_ExplicitValues(t *testing.T) {
	off, err := loadRCAYAML(t, "sre:\n  llm:\n    enabled: false\n")
	if err != nil || off.SRE.LLM.Enabled {
		t.Fatalf("enabled: false = %+v (%v)", off.SRE.LLM, err)
	}
	on, err := loadRCAYAML(t, "sre:\n  llm:\n    enabled: true\n")
	if err != nil || !on.SRE.LLM.Enabled {
		t.Fatalf("enabled: true = %+v (%v)", on.SRE.LLM, err)
	}
	for _, bad := range []string{"sre:\n  llm:\n    enabld: false\n",
		"sre:\n  llm:\n    enabled: maybe\n", "sre:\n  llm: false\n"} {
		if _, err := loadRCAYAML(t, bad); err == nil {
			t.Errorf("accepted invalid sre.llm config %q", bad)
		}
	}
}

func TestSRELLM_DocTagSaysOnByDefault(t *testing.T) {
	doc := collectDocTags(t, DefaultConfig())["sre.llm.enabled"]
	for _, want := range []string{"Default: true", "false"} {
		if !strings.Contains(doc, want) {
			t.Errorf("sre.llm.enabled doc %q lacks %q", doc, want)
		}
	}
}
