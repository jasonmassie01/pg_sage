package rubric

import "testing"

// pub-01: the exported thresholds and source list docs/rubric.md is
// generated from.

func TestPublished_ThresholdsAndSourcesMatchTheGrade(t *testing.T) {
	if MinCoveragePct != 60 || MinKnownDimensions != 4 || WarnWeightFraction != 0.5 ||
		CriticalCap != "D" {
		t.Fatalf("thresholds %v %v %v %v", MinCoveragePct, MinKnownDimensions,
			WarnWeightFraction, CriticalCap)
	}
	seen := map[Source]bool{}
	for _, s := range Sources() {
		if seen[s] {
			t.Fatalf("source %s listed twice", s)
		}
		seen[s] = true
	}
	for _, d := range V1().Dimensions {
		for _, c := range d.Checks {
			if !seen[c.Source] {
				t.Errorf("check %s uses source %s, which Sources() lacks", c.ID, c.Source)
			}
		}
	}
	if l := Letters(); len(l) != 5 || l[0] != "A" || l[4] != "F" {
		t.Fatalf("letters %v, want A to F", l)
	}
}
