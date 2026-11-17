package config

import (
	"strings"
	"testing"
)

// review.large_table_bytes (review-02d): the size from which the schema
// debt generator calls an unpartitioned table with a time-ordered key
// giant, 10 GiB by default, at least 1 GiB and at most 1 PiB.
// No concurrent access tests: validation is a pure function of the value.

func TestReviewLargeTableBytes_DefaultsTo10GiBWithoutAFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const tenGiB = 10737418240
	if cfg.Review.LargeTableBytes != tenGiB ||
		DefaultConfig().Review.LargeTableBytes != tenGiB ||
		DefaultReviewLargeTableBytes != tenGiB {
		t.Fatalf("large_table_bytes = %d / %d / %d, want 10 GiB", cfg.Review.LargeTableBytes,
			DefaultConfig().Review.LargeTableBytes, DefaultReviewLargeTableBytes)
	}
}

func TestReviewLargeTableBytes_PartialSectionKeepsTheDefault(t *testing.T) {
	c, err := loadRCAYAML(t, "review:\n  window_days: 30\n")
	if err != nil || c.Review.LargeTableBytes != 10737418240 {
		t.Fatalf("review = %+v (%v), want the 10 GiB default", c.Review, err)
	}
}

func TestReviewLargeTableBytes_Boundaries(t *testing.T) {
	for v, want := range map[string]int64{"1073741824": 1 << 30,
		"1125899906842624": 1 << 50, "53687091200": 50 << 30} {
		c, err := loadRCAYAML(t, "review:\n  large_table_bytes: "+v+"\n")
		if err != nil || c.Review.LargeTableBytes != want {
			t.Errorf("%s = %+v %v, want %d", v, c.Review, err, want)
		}
	}
	for _, v := range []string{"1073741823", "0", "-1", "1125899906842625"} {
		_, err := loadRCAYAML(t, "review:\n  large_table_bytes: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "review.large_table_bytes") ||
			!strings.Contains(err.Error(), "1073741824-1125899906842624") {
			t.Errorf("%s: error %v does not name the key and its range", v, err)
		}
	}
}

func TestReviewLargeTableBytes_DocTagLifecycleAndClass(t *testing.T) {
	tags := collectDocTags(t, DefaultConfig())
	for _, fragment := range []string{"Default: 10737418240", "10 GiB"} {
		if !strings.Contains(tags["review.large_table_bytes"], fragment) {
			t.Errorf("doc %q lacks %q", tags["review.large_table_bytes"], fragment)
		}
	}
	lc, ok := LookupFieldLifecycle("review.large_table_bytes")
	if !ok || lc.Lifecycle != LifecycleRestart {
		t.Errorf("lifecycle = %+v (%v), want restart", lc, ok)
	}
	class, ok := KeyClassOf("review.large_table_bytes")
	if !ok || class != KeyOperatorPreference {
		t.Errorf("class = %q (%v), want operator_preference", class, ok)
	}
}
