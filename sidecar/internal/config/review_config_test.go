package config

import (
	"strings"
	"testing"
)

// review.enabled and review.grade_interval_hours (review-01a): the
// deterministic, read-only grade is on by default and runs every 24 hours
// (at least every hour, at most every 30 days).
// No concurrent access tests: validation is a pure function of the value.

func TestReviewConfig_DefaultsWithoutAFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, r := range map[string]ReviewConfig{"Load": cfg.Review,
		"DefaultConfig": DefaultConfig().Review} {
		if !r.Enabled || r.GradeIntervalHours != 24 {
			t.Errorf("%s review = %+v, want enabled every 24 hours", name, r)
		}
	}
	if DefaultReviewGradeIntervalHours != 24 {
		t.Fatalf("DefaultReviewGradeIntervalHours = %d", DefaultReviewGradeIntervalHours)
	}
}

func TestReviewConfig_PartialSectionKeepsTheDefault(t *testing.T) {
	c, err := loadRCAYAML(t, "review:\n  enabled: false\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Review.Enabled || c.Review.GradeIntervalHours != 24 {
		t.Fatalf("review = %+v, want disabled with the 24 hour default", c.Review)
	}
}

func TestReviewConfig_IntervalBoundaries(t *testing.T) {
	for v, want := range map[string]int{"1": 1, "720": 720, "6": 6} {
		c, err := loadRCAYAML(t, "review:\n  grade_interval_hours: "+v+"\n")
		if err != nil || c.Review.GradeIntervalHours != want {
			t.Errorf("%s = %+v %v, want %d", v, c, err, want)
		}
	}
	for _, v := range []string{"0", "-1", "721"} {
		_, err := loadRCAYAML(t, "review:\n  grade_interval_hours: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "review.grade_interval_hours") ||
			!strings.Contains(err.Error(), "1-720") {
			t.Errorf("%s: error %v does not name the key and its range", v, err)
		}
	}
}

func TestReviewConfig_DocTagLifecycleAndClass(t *testing.T) {
	tags := collectDocTags(t, DefaultConfig())
	for key, fragments := range map[string][]string{
		"review.enabled":              {"Default: true", "read-only"},
		"review.grade_interval_hours": {"Default: 24", "1-720"}} {
		for _, fragment := range fragments {
			if !strings.Contains(tags[key], fragment) {
				t.Errorf("%s doc %q lacks %q", key, tags[key], fragment)
			}
		}
		lc, ok := LookupFieldLifecycle(key)
		if !ok || lc.Lifecycle != LifecycleRestart {
			t.Errorf("%s lifecycle = %+v (%v), want restart", key, lc, ok)
		}
		if class, ok := KeyClassOf(key); !ok || class != KeyOperatorPreference {
			t.Errorf("%s class = %q (%v), want operator_preference", key, class, ok)
		}
	}
}
