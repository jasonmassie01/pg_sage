package config

import (
	"strings"
	"testing"
)

// review.gate (review-05a): the thresholds of G-M1 and G-M2, decision D11's
// proposals (50 reviews, 10 points, 2%), and the databases that never count.
// No concurrent access tests: loading and validation are pure functions of
// the file and the environment.

func TestReviewGate_DefaultsWithoutAFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, g := range map[string]ReviewGateConfig{"Load": cfg.Review.Gate,
		"DefaultConfig": DefaultConfig().Review.Gate} {
		if g.MinReviews != 50 || g.MaxGapPoints != 10 || g.MaxRevertPct != 2 ||
			len(g.FixtureDatabases) != 0 {
			t.Fatalf("%s: review.gate = %+v, want 50 reviews, 10 points, 2%%, no fixtures",
				name, g)
		}
	}
}

func TestReviewGate_PartialSectionKeepsTheDefaults(t *testing.T) {
	c, err := loadRCAYAML(t, "review:\n  gate:\n    min_reviews: 60\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	g := c.Review.Gate
	if g.MinReviews != 60 || g.MaxGapPoints != 10 || g.MaxRevertPct != 2 {
		t.Fatalf("review.gate = %+v, want 60 reviews and the other defaults", g)
	}
	c, err = loadRCAYAML(t, "review:\n  gate:\n    max_gap_points: 0\n"+
		"    max_revert_pct: 0\n    fixture_databases: [bench1, gameday]\n")
	if err != nil {
		t.Fatalf("load zeros: %v", err)
	}
	g = c.Review.Gate
	if g.MaxGapPoints != 0 || g.MaxRevertPct != 0 || g.MinReviews != 50 ||
		strings.Join(g.FixtureDatabases, ",") != "bench1,gameday" {
		t.Fatalf("review.gate = %+v, want explicit zeros kept and two fixtures", g)
	}
}

func TestReviewGate_RefusesNegativeAndOutOfRangeThresholds(t *testing.T) {
	cases := map[string]string{
		"review:\n  gate:\n    min_reviews: 0\n":        "review.gate.min_reviews",
		"review:\n  gate:\n    min_reviews: -1\n":       "review.gate.min_reviews",
		"review:\n  gate:\n    min_reviews: 10001\n":    "review.gate.min_reviews",
		"review:\n  gate:\n    max_gap_points: -0.5\n":  "review.gate.max_gap_points",
		"review:\n  gate:\n    max_gap_points: 100.1\n": "review.gate.max_gap_points",
		"review:\n  gate:\n    max_revert_pct: -2\n":    "review.gate.max_revert_pct",
		"review:\n  gate:\n    max_revert_pct: 101\n":   "review.gate.max_revert_pct",
		"review:\n  gate:\n    fixture_databases: [\"\"]\n": "review.gate." +
			"fixture_databases",
	}
	for body, key := range cases {
		_, err := loadRCAYAML(t, body)
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%q: error %v does not name %s", body, err, key)
		}
	}
}

func TestReviewGate_BoundariesAreAccepted(t *testing.T) {
	for _, body := range []string{
		"review:\n  gate:\n    min_reviews: 1\n",
		"review:\n  gate:\n    min_reviews: 10000\n",
		"review:\n  gate:\n    max_gap_points: 100\n",
		"review:\n  gate:\n    max_revert_pct: 100\n",
	} {
		if _, err := loadRCAYAML(t, body); err != nil {
			t.Errorf("%q refused: %v", body, err)
		}
	}
}

func TestReviewGate_DocTagsSayTheyAreOwnerProposals(t *testing.T) {
	tags := collectDocTags(t, DefaultConfig())
	for key, fragments := range map[string][]string{
		"review.gate.min_reviews":       {"Default: 50", "D11"},
		"review.gate.max_gap_points":    {"Default: 10", "D11"},
		"review.gate.max_revert_pct":    {"Default: 2", "D11"},
		"review.gate.fixture_databases": {"never count"},
	} {
		for _, fragment := range fragments {
			if !strings.Contains(tags[key], fragment) {
				t.Errorf("%s doc %q lacks %q", key, tags[key], fragment)
			}
		}
	}
}
