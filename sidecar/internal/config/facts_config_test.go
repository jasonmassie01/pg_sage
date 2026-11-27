package config

import (
	"strconv"
	"strings"
	"testing"
)

// facts-08: facts.confirmed_absence_days is how long a confirmed operator
// or model fact keeps binding while its subject is absent, and how long
// after such an expiry the same confirmation comes back with the subject.

// Default-value masking guard: with no config file the key is 7, not 0.
func TestFactsConfig_DefaultsWithoutAConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Facts.ConfirmedAbsenceDays != 7 || DefaultFactsConfirmedAbsenceDays != 7 {
		t.Fatalf("facts.confirmed_absence_days = %d (const %d), want 7",
			cfg.Facts.ConfirmedAbsenceDays, DefaultFactsConfirmedAbsenceDays)
	}
	if DefaultConfig().Facts.ConfirmedAbsenceDays != 7 {
		t.Fatalf("DefaultConfig facts = %+v", DefaultConfig().Facts)
	}
}

// A missing key inside a present section keeps the default.
func TestFactsConfig_MissingKeyKeepsTheDefault(t *testing.T) {
	cfg, err := loadRCAYAML(t, "facts: {}\n")
	if err != nil || cfg.Facts.ConfirmedAbsenceDays != 7 {
		t.Fatalf("facts: {} -> %+v %v, want 7", cfg, err)
	}
}

func TestFactsConfig_Bounds(t *testing.T) {
	for _, v := range []string{"1", "7", "90"} {
		cfg, err := loadRCAYAML(t, "facts:\n  confirmed_absence_days: "+v+"\n")
		if err != nil {
			t.Fatalf("%s refused: %v", v, err)
		}
		if got := cfg.Facts.ConfirmedAbsenceDays; got == 0 || v != strconv.Itoa(got) {
			t.Fatalf("%s loaded as %d", v, got)
		}
	}
	for _, v := range []string{"0", "91", "-1"} {
		_, err := loadRCAYAML(t, "facts:\n  confirmed_absence_days: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "facts.confirmed_absence_days") ||
			!strings.Contains(err.Error(), "1-90") {
			t.Fatalf("%s: want an error naming the key and its range, got %v", v, err)
		}
	}
}

func TestFactsConfig_DocAndLifecycle(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	if !strings.Contains(docs["facts.confirmed_absence_days"], "Default: 7") {
		t.Fatalf("doc %q lacks the default", docs["facts.confirmed_absence_days"])
	}
	if lc, ok := LookupFieldLifecycle("facts.confirmed_absence_days"); !ok ||
		lc.Lifecycle != LifecycleRestart {
		t.Fatalf("lifecycle = %+v (%v), want restart", lc, ok)
	}
	if c, ok := KeyClassOf("facts.confirmed_absence_days"); !ok || c != KeyOperatorPreference {
		t.Fatalf("class = %q (%v), want operator_preference", c, ok)
	}
}
