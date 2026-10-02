package config

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Sage SRE M6 runways (sre.runways.*): sampling and pre-incident
// investigations are on by default (they are read-only); horizons and
// trend minimums have documented defaults that an absent file, a partial
// section or explicit zeros cannot mask.

func TestRunwayDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := cfg.SRE.Runways
	if !r.Enabled || !r.Investigate || r.IntervalSeconds != 60 || r.LookbackHours != 6 ||
		r.MinSamples != 10 || r.MinSpanMinutes != 30 || r.WraparoundHorizonHours != 336 ||
		r.WraparoundCriticalHours != 72 || r.DiskHorizonHours != 72 ||
		r.DiskCriticalHours != 24 || r.SequenceHorizonDays != 30 ||
		r.SequenceCriticalDays != 7 || r.SampleRetentionHours != 48 {
		t.Fatalf("runway defaults = %+v", r)
	}
	if r.Interval() != time.Minute || r.Lookback() != 6*time.Hour ||
		r.MinSpan() != 30*time.Minute || r.WraparoundHorizon() != 14*24*time.Hour ||
		r.WraparoundCritical() != 72*time.Hour || r.DiskHorizon() != 72*time.Hour ||
		r.DiskCritical() != 24*time.Hour || r.SequenceHorizon() != 30*24*time.Hour ||
		r.SequenceCritical() != 7*24*time.Hour || r.SampleRetention() != 48*time.Hour {
		t.Fatalf("runway durations wrong: %+v", r)
	}
	if DefaultConfig().SRE.Runways != r {
		t.Fatal("DefaultConfig and Load(nil) disagree on runway defaults")
	}
}

func TestRunwayDefaults_PartialSectionKeepsTheRest(t *testing.T) {
	cfg, err := loadRCAYAML(t, "sre:\n  runways:\n    investigate: false\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := cfg.SRE.Runways
	if r.Investigate || !r.Enabled || r.MinSamples != 10 || r.DiskHorizonHours != 72 {
		t.Fatalf("runways = %+v, want only investigate turned off", r)
	}
	off, err := loadRCAYAML(t, "sre:\n  runways:\n    enabled: false\n")
	if err != nil || off.SRE.Runways.Enabled {
		t.Fatalf("enabled: false = %+v (%v)", off.SRE.Runways, err)
	}
}

func TestRunwayConfig_Boundaries(t *testing.T) {
	cases := []struct {
		key    string
		value  int
		wantOK bool
	}{
		{"interval_seconds", 0, false}, {"interval_seconds", 14, false},
		{"interval_seconds", 15, true}, {"interval_seconds", 3600, true},
		{"interval_seconds", 3601, false},
		{"lookback_hours", 0, false}, {"lookback_hours", 1, true},
		{"lookback_hours", 48, true}, {"lookback_hours", 49, false}, // > retention
		{"min_samples", 2, false}, {"min_samples", 3, true}, {"min_samples", 1000, true},
		{"min_samples", 1001, false},
		{"min_span_minutes", 0, false}, {"min_span_minutes", 1, true},
		{"min_span_minutes", 360, true}, {"min_span_minutes", 361, false}, // > lookback
		{"wraparound_horizon_hours", 71, false}, // under its critical horizon
		{"wraparound_horizon_hours", 72, true}, {"wraparound_horizon_hours", 8760, true},
		{"wraparound_horizon_hours", 8761, false},
		{"wraparound_critical_hours", 0, false}, {"wraparound_critical_hours", 336, true},
		{"wraparound_critical_hours", 337, false},
		{"disk_horizon_hours", 23, false}, {"disk_horizon_hours", 24, true},
		{"disk_critical_hours", 0, false}, {"disk_critical_hours", 73, false},
		{"sequence_horizon_days", 6, false}, {"sequence_horizon_days", 3650, true},
		{"sequence_horizon_days", 3651, false},
		{"sequence_critical_days", 0, false}, {"sequence_critical_days", 30, true},
		{"sample_retention_hours", 5, false}, {"sample_retention_hours", 6, true},
		{"sample_retention_hours", 720, true}, {"sample_retention_hours", 721, false},
	}
	for _, c := range cases {
		t.Run(c.key+"="+strconv.Itoa(c.value), func(t *testing.T) {
			_, err := loadRCAYAML(t, "sre:\n  runways:\n    "+c.key+": "+
				strconv.Itoa(c.value)+"\n")
			if c.wantOK && err != nil {
				t.Fatalf("%s=%d rejected: %v", c.key, c.value, err)
			}
			if !c.wantOK {
				if err == nil {
					t.Fatalf("%s=%d accepted", c.key, c.value)
				}
				if !strings.Contains(err.Error(), "sre.runways.") {
					t.Fatalf("error must name the sre.runways key: %v", err)
				}
			}
		})
	}
}

func TestRunwayConfig_UnknownKeyRejected(t *testing.T) {
	if _, err := loadRCAYAML(t, "sre:\n  runways:\n    horizon_days: 3\n"); err == nil {
		t.Fatal("an unknown sre.runways key was accepted")
	}
}

func TestRunwayConfig_KeysHaveDocTags(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range []string{"enabled", "investigate", "interval_seconds",
		"lookback_hours", "min_samples", "min_span_minutes", "wraparound_horizon_hours",
		"wraparound_critical_hours", "disk_horizon_hours", "disk_critical_hours",
		"sequence_horizon_days", "sequence_critical_days", "sample_retention_hours"} {
		if strings.TrimSpace(docs["sre.runways."+key]) == "" {
			t.Errorf("sre.runways.%s has no doc tag", key)
		}
	}
}
