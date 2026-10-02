package config

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Dogfood lifeos-1 keys: sre.runways.sequence_interval_seconds (sequences
// are sampled on a slower cadence than WAL/XID) and rca.stale_after_hours
// (an incident not re-detected for this long resolves as stale). Both
// have defaults that an absent file or a partial section cannot mask.

func TestDogfoodDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := cfg.SRE.Runways
	if r.SequenceIntervalSeconds != 600 || r.SequenceInterval() != 10*time.Minute {
		t.Fatalf("sequence interval = %d (%s), want 600 s", r.SequenceIntervalSeconds,
			r.SequenceInterval())
	}
	if cfg.RCA.StaleAfterHours != 24 || cfg.RCA.StaleAfter() != 24*time.Hour {
		t.Fatalf("rca stale after = %d (%s), want 24 h", cfg.RCA.StaleAfterHours,
			cfg.RCA.StaleAfter())
	}
	d := DefaultConfig()
	if d.SRE.Runways.SequenceIntervalSeconds != 600 || d.RCA.StaleAfterHours != 24 {
		t.Fatal("DefaultConfig and Load(nil) disagree")
	}
}

func TestDogfoodDefaults_PartialSectionsKeepTheRest(t *testing.T) {
	cfg, err := loadRCAYAML(t, "rca:\n  dedup_window_minutes: 45\n"+
		"sre:\n  runways:\n    interval_seconds: 120\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RCA.StaleAfterHours != 24 || cfg.SRE.Runways.SequenceIntervalSeconds != 600 {
		t.Fatalf("stale %d, sequence interval %d: defaults masked",
			cfg.RCA.StaleAfterHours, cfg.SRE.Runways.SequenceIntervalSeconds)
	}
}

// A literal RCAConfig (tests, other constructors) without the key still
// gets the default, never "resolve immediately".
func TestRCAStaleAfter_ZeroValueMeansDefault(t *testing.T) {
	if got := (&RCAConfig{}).StaleAfter(); got != 24*time.Hour {
		t.Fatalf("zero-value stale after = %s, want 24h", got)
	}
	if got := (&RCAConfig{StaleAfterHours: 3}).StaleAfter(); got != 3*time.Hour {
		t.Fatalf("stale after = %s, want 3h", got)
	}
}

func TestSequenceInterval_Boundaries(t *testing.T) {
	cases := []struct {
		yaml   string
		wantOK bool
	}{
		{"sequence_interval_seconds: 0", false},
		{"sequence_interval_seconds: -1", false},
		{"sequence_interval_seconds: 59", false}, // under interval_seconds
		{"sequence_interval_seconds: 60", true},
		{"sequence_interval_seconds: 2400", true},  // 9 x 2400 s = the 6 h lookback
		{"sequence_interval_seconds: 2401", false}, // min_samples no longer fit
		{"sequence_interval_seconds: 86400\n    min_samples: 3\n    lookback_hours: 48", true},
		{"sequence_interval_seconds: 86401\n    min_samples: 3\n    lookback_hours: 168\n" +
			"    sample_retention_hours: 720", false},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.yaml, "\n", " "), func(t *testing.T) {
			_, err := loadRCAYAML(t, "sre:\n  runways:\n    "+c.yaml+"\n")
			if c.wantOK && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if !c.wantOK && (err == nil ||
				!strings.Contains(err.Error(), "sre.runways.sequence_interval_seconds")) {
				t.Fatalf("err = %v, want the key named", err)
			}
		})
	}
}

func TestRCAStaleAfter_Boundaries(t *testing.T) {
	cases := []struct {
		hours  int
		dedup  int
		wantOK bool
	}{
		{0, 30, false}, {-1, 30, false}, {1, 30, true}, {24, 30, true},
		{8760, 30, true}, {8761, 30, false},
		{1, 60, true}, {1, 61, false}, // must cover the dedup window
	}
	for _, c := range cases {
		name := strconv.Itoa(c.hours) + "h/dedup" + strconv.Itoa(c.dedup)
		t.Run(name, func(t *testing.T) {
			_, err := loadRCAYAML(t, "rca:\n  stale_after_hours: "+strconv.Itoa(c.hours)+
				"\n  dedup_window_minutes: "+strconv.Itoa(c.dedup)+"\n")
			if c.wantOK && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if !c.wantOK && (err == nil ||
				!strings.Contains(err.Error(), "rca.stale_after_hours")) {
				t.Fatalf("err = %v, want rca.stale_after_hours named", err)
			}
		})
	}
}

func TestDogfoodKeys_HaveDocTags(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range []string{"sre.runways.sequence_interval_seconds",
		"rca.stale_after_hours"} {
		if strings.TrimSpace(docs[key]) == "" {
			t.Errorf("%s has no doc tag", key)
		}
	}
}
