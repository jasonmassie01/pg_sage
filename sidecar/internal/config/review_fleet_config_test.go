package config

import (
	"strings"
	"testing"
)

// review.triage_model and review.fleet_deep_max (review-04b): the fleet
// triage tier's model (empty: the general client's) and the cap of deep
// reviews a fleet schedules per calendar month, 30 by default.
// No concurrent access tests: validation is a pure function of the value.

func TestReviewFleet_DefaultsWithoutAFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, r := range []ReviewConfig{cfg.Review, DefaultConfig().Review} {
		if r.FleetDeepMax != 30 || r.TriageModel != "" {
			t.Fatalf("review = %+v, want fleet_deep_max 30 and no triage model", r)
		}
	}
	if DefaultReviewFleetDeepMax != 30 {
		t.Fatalf("default fleet_deep_max %d", DefaultReviewFleetDeepMax)
	}
}

func TestReviewFleet_PartialSectionKeepsTheDefaults(t *testing.T) {
	c, err := loadRCAYAML(t, "review:\n  triage_model: small-model\n")
	if err != nil || c.Review.TriageModel != "small-model" || c.Review.FleetDeepMax != 30 {
		t.Fatalf("review = %+v (%v), want the triage model and the 30 default", c.Review, err)
	}
}

func TestReviewFleet_Boundaries(t *testing.T) {
	ok := map[string]func(ReviewConfig) bool{
		"fleet_deep_max: 0":    func(r ReviewConfig) bool { return r.FleetDeepMax == 0 },
		"fleet_deep_max: 1":    func(r ReviewConfig) bool { return r.FleetDeepMax == 1 },
		"fleet_deep_max: 1000": func(r ReviewConfig) bool { return r.FleetDeepMax == 1000 },
	}
	for line, check := range ok {
		c, err := loadRCAYAML(t, "review:\n  "+line+"\n")
		if err != nil || !check(c.Review) {
			t.Errorf("%s = %+v %v, want accepted", line, c.Review, err)
		}
	}
	bad := map[string]string{
		"fleet_deep_max: -1":                                 "0-1000",
		"fleet_deep_max: 1001":                               "0-1000",
		"triage_model: \" \"":                                "blank",
		"triage_model: \"" + strings.Repeat("m", 201) + "\"": "200",
	}
	for line, want := range bad {
		_, err := loadRCAYAML(t, "review:\n  "+line+"\n")
		key := "review." + strings.SplitN(line, ":", 2)[0]
		if err == nil || !strings.Contains(err.Error(), key) ||
			!strings.Contains(err.Error(), want) {
			t.Errorf("%.40s: error %v does not name %s and %q", line, err, key, want)
		}
	}
}

func TestReviewFleet_DocTagsLifecycleAndClass(t *testing.T) {
	tags := collectDocTags(t, DefaultConfig())
	for key, frag := range map[string]string{"review.fleet_deep_max": "Default: 30",
		"review.triage_model": "general"} {
		if !strings.Contains(tags[key], frag) {
			t.Errorf("doc of %s = %q, lacks %q", key, tags[key], frag)
		}
		lc, ok := LookupFieldLifecycle(key)
		if !ok || lc.Lifecycle != LifecycleRestart {
			t.Errorf("%s lifecycle = %+v (%v), want restart", key, lc, ok)
		}
		class, ok := KeyClassOf(key)
		if !ok || class != KeyOperatorPreference {
			t.Errorf("%s class = %q (%v), want operator_preference", key, class, ok)
		}
	}
}
