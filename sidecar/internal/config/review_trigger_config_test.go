package config

import (
	"strings"
	"testing"
)

// review.calendar_days and review.min_interval_hours (review-04a): the
// calendar trigger's period, 30 days by default, and the coalescing
// window of event triggers, 24 hours by default.
// No concurrent access tests: validation is a pure function of the value.

func TestReviewTriggers_DefaultsWithoutAFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, r := range []ReviewConfig{cfg.Review, DefaultConfig().Review} {
		if r.CalendarDays != 30 || r.MinIntervalHours != 24 ||
			r.LargeTableBytes != 10<<30 {
			t.Fatalf("review = %+v, want calendar_days 30, min_interval_hours 24 and "+
				"large_table_bytes 10 GiB", r)
		}
	}
	if DefaultReviewCalendarDays != 30 || DefaultReviewMinIntervalHours != 24 {
		t.Fatalf("defaults %d / %d", DefaultReviewCalendarDays, DefaultReviewMinIntervalHours)
	}
}

func TestReviewTriggers_PartialSectionKeepsTheDefaults(t *testing.T) {
	c, err := loadRCAYAML(t, "review:\n  calendar_days: 7\n")
	if err != nil || c.Review.CalendarDays != 7 || c.Review.MinIntervalHours != 24 {
		t.Fatalf("review = %+v (%v), want calendar 7 and the 24 h default", c.Review, err)
	}
}

func TestReviewTriggers_Boundaries(t *testing.T) {
	ok := map[string]func(ReviewConfig) bool{
		"calendar_days: 1":        func(r ReviewConfig) bool { return r.CalendarDays == 1 },
		"calendar_days: 365":      func(r ReviewConfig) bool { return r.CalendarDays == 365 },
		"min_interval_hours: 1":   func(r ReviewConfig) bool { return r.MinIntervalHours == 1 },
		"min_interval_hours: 168": func(r ReviewConfig) bool { return r.MinIntervalHours == 168 },
	}
	for line, check := range ok {
		c, err := loadRCAYAML(t, "review:\n  "+line+"\n")
		if err != nil || !check(c.Review) {
			t.Errorf("%s = %+v %v, want accepted", line, c.Review, err)
		}
	}
	bad := map[string]string{
		"calendar_days: 0": "1-365", "calendar_days: 366": "1-365",
		"calendar_days: -3": "1-365", "min_interval_hours: 0": "1-168",
		"min_interval_hours: 169": "1-168",
	}
	for line, rng := range bad {
		_, err := loadRCAYAML(t, "review:\n  "+line+"\n")
		key := "review." + strings.SplitN(line, ":", 2)[0]
		if err == nil || !strings.Contains(err.Error(), key) ||
			!strings.Contains(err.Error(), rng) {
			t.Errorf("%s: error %v does not name %s and its range %s", line, err, key, rng)
		}
	}
}

func TestReviewTriggers_DocTagsLifecycleAndClass(t *testing.T) {
	tags := collectDocTags(t, DefaultConfig())
	for key, frag := range map[string]string{"review.calendar_days": "Default: 30",
		"review.min_interval_hours": "Default: 24"} {
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
