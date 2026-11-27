package config

import (
	"strings"
	"testing"
)

// review.window_days (review-02a): the long window the review generators
// read, 90 days by default, at least 14 (the shortest workload pack) and
// at most 100 (the longest).
// No concurrent access tests: validation is a pure function of the value.

func TestReviewWindowDays_DefaultsTo90WithoutAFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Review.WindowDays != 90 || DefaultConfig().Review.WindowDays != 90 ||
		DefaultReviewWindowDays != 90 {
		t.Fatalf("window_days = %d / %d / %d, want 90", cfg.Review.WindowDays,
			DefaultConfig().Review.WindowDays, DefaultReviewWindowDays)
	}
}

func TestReviewWindowDays_PartialSectionKeepsTheDefault(t *testing.T) {
	c, err := loadRCAYAML(t, "review:\n  grade_interval_hours: 6\n")
	if err != nil || c.Review.WindowDays != 90 {
		t.Fatalf("review = %+v (%v), want the 90 day default", c.Review, err)
	}
}

func TestReviewWindowDays_Boundaries(t *testing.T) {
	for v, want := range map[string]int{"14": 14, "100": 100, "28": 28} {
		c, err := loadRCAYAML(t, "review:\n  window_days: "+v+"\n")
		if err != nil || c.Review.WindowDays != want {
			t.Errorf("%s = %+v %v, want %d", v, c.Review, err, want)
		}
	}
	for _, v := range []string{"13", "0", "-1", "101"} {
		_, err := loadRCAYAML(t, "review:\n  window_days: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "review.window_days") ||
			!strings.Contains(err.Error(), "14-100") {
			t.Errorf("%s: error %v does not name the key and its range", v, err)
		}
	}
}

func TestReviewWindowDays_DocTagLifecycleAndClass(t *testing.T) {
	tags := collectDocTags(t, DefaultConfig())
	for _, fragment := range []string{"Default: 90", "14-100"} {
		if !strings.Contains(tags["review.window_days"], fragment) {
			t.Errorf("doc %q lacks %q", tags["review.window_days"], fragment)
		}
	}
	lc, ok := LookupFieldLifecycle("review.window_days")
	if !ok || lc.Lifecycle != LifecycleRestart {
		t.Errorf("lifecycle = %+v (%v), want restart", lc, ok)
	}
	if class, ok := KeyClassOf("review.window_days"); !ok || class != KeyOperatorPreference {
		t.Errorf("class = %q (%v), want operator_preference", class, ok)
	}
}
