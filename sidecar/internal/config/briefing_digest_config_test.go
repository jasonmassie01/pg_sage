package config

import (
	"math"
	"strings"
	"testing"
)

// ux-02b: briefing.only_when_changed (default true) sends a digest only
// when it differs from the last one delivered; briefing.dba_hour_rate_usd
// (default 0) prices saved DBA hours. Both apply live through the
// briefing owner.
// No concurrent access tests: these are load-time checks and lookups.

// Default masking: without a file or the key, only_when_changed is true.
func TestBriefingOnlyWhenChangedDefaultsToTrue(t *testing.T) {
	if !DefaultConfig().Briefing.OnlyWhenChanged {
		t.Fatal("DefaultConfig briefing.only_when_changed = false, want true")
	}
	if !loadNoConfigFile(t).Briefing.OnlyWhenChanged {
		t.Fatal("Load without a file: briefing.only_when_changed = false, want true")
	}
	partial := loadYAMLConfig(t, "briefing:\n  schedule: \"0 7 * * *\"\n")
	if !partial.Briefing.OnlyWhenChanged {
		t.Fatal("a briefing section without the key turned only_when_changed off")
	}
	off := loadYAMLConfig(t, "briefing:\n  only_when_changed: false\n")
	if off.Briefing.OnlyWhenChanged {
		t.Fatal("only_when_changed: false loaded as true")
	}
	if DefaultConfig().Briefing.DBAHourRateUSD != 0 {
		t.Fatal("dba_hour_rate_usd defaults to a price: pg_sage ships no rates (market-04)")
	}
}

// Invalid input and boundaries: a negative or non-finite rate is refused,
// naming the key; zero is valid (hours only).
func TestBriefingDBAHourRateValidation(t *testing.T) {
	for _, bad := range []float64{-0.01, math.Inf(1), math.NaN()} {
		b := DefaultConfig().Briefing
		b.DBAHourRateUSD = bad
		if err := b.validate(); err == nil ||
			!strings.Contains(err.Error(), "briefing.dba_hour_rate_usd") {
			t.Errorf("rate %v: err = %v, want one naming the key", bad, err)
		}
	}
	for _, ok := range []float64{0, 150} {
		b := DefaultConfig().Briefing
		b.DBAHourRateUSD = ok
		if err := b.validate(); err != nil {
			t.Errorf("rate %v: %v, want valid", ok, err)
		}
	}
}

func TestBriefingDigestKeysAreLiveAndDocumented(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	for _, path := range []string{"briefing.only_when_changed",
		"briefing.dba_hour_rate_usd"} {
		lc, ok := LookupFieldLifecycle(path)
		if !ok || lc.Lifecycle != LifecycleReconfigure || lc.Owner != BriefingOwner {
			t.Errorf("%s lifecycle = %+v (%v), want reconfigure by briefing", path, lc, ok)
		}
		if docs[path] == "" {
			t.Errorf("%s has no doc tag", path)
		}
	}
}
