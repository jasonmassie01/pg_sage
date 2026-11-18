package rubric

import (
	"fmt"
	"math/rand"
	"testing"
)

// review-01a, spec CHECK "a grade never improves because a check could not
// run": 10,000 seeded random rubrics and results, where the only change
// from prev to cur is some checks turning unknown. No concurrent access
// tests: Compute is pure.

const propertyCases = 10000

var allStatuses = []Status{StatusPass, StatusWarn, StatusFail, StatusUnknown,
	StatusNotApplicable}

func randomCase(rng *rand.Rand) (Rubric, []CheckResult, []CheckResult) {
	r := Rubric{Version: "prop/1"}
	var prev, cur []CheckResult
	for d := range 1 + rng.Intn(8) {
		dm := Dimension{ID: fmt.Sprintf("d%d", d), Title: "D"}
		for c := range 1 + rng.Intn(6) {
			id := fmt.Sprintf("d%d.c%d", d, c)
			dm.Checks = append(dm.Checks, CheckSpec{ID: id, Source: SourceFinding,
				Ref: "temp_spill", Weight: 1 + rng.Intn(5), Critical: rng.Intn(4) == 0})
			st := allStatuses[rng.Intn(len(allStatuses))]
			prev = append(prev, CheckResult{Check: id, Status: st})
			if st != StatusNotApplicable && rng.Intn(3) == 0 {
				st = StatusUnknown
			}
			cur = append(cur, CheckResult{Check: id, Status: st, Reason: "timeout"})
		}
		r.Dimensions = append(r.Dimensions, dm)
	}
	return r, prev, cur
}

// rank orders letters; unknown ranks below F, so turning unknown is never
// better.
func rank(l Letter) int {
	switch l {
	case "A":
		return 5
	case "B":
		return 4
	case "C":
		return 3
	case "D":
		return 2
	case "F":
		return 1
	}
	return 0
}

func TestProperty_UnknownNeverImprovesAGrade(t *testing.T) {
	rng := rand.New(rand.NewSource(20261117))
	for i := range propertyCases {
		r, prev, cur := randomCase(rng)
		if msg := monotone(Compute(r, prev), Compute(r, cur)); msg != "" {
			t.Fatalf("case %d: %s\nprev %+v\ncur %+v", i, msg, prev, cur)
		}
	}
}

func monotone(p, c Grade) string {
	const eps = 1e-9
	if c.OverallScore > p.OverallScore+eps {
		return fmt.Sprintf("overall score rose %.2f -> %.2f", p.OverallScore, c.OverallScore)
	}
	if rank(c.Overall) > rank(p.Overall) {
		return fmt.Sprintf("overall letter rose %q -> %q", p.Overall, c.Overall)
	}
	if c.CoveragePct > p.CoveragePct+eps {
		return fmt.Sprintf("coverage rose %.2f -> %.2f", p.CoveragePct, c.CoveragePct)
	}
	for i := range p.Dimensions {
		pd, cd := p.Dimensions[i], c.Dimensions[i]
		if cd.Score > pd.Score+eps || rank(cd.Letter) > rank(pd.Letter) {
			return fmt.Sprintf("%s rose %.2f %q -> %.2f %q", pd.Dimension, pd.Score,
				pd.Letter, cd.Score, cd.Letter)
		}
	}
	return ""
}

// The property is not vacuous: the cases include grades that drop.
func TestProperty_CasesExerciseRealDrops(t *testing.T) {
	rng := rand.New(rand.NewSource(20261117))
	drops, graded := 0, 0
	for range propertyCases {
		r, prev, cur := randomCase(rng)
		p, c := Compute(r, prev), Compute(r, cur)
		if p.Overall != "" {
			graded++
		}
		if rank(c.Overall) < rank(p.Overall) {
			drops++
		}
	}
	if drops < 500 || graded < 1000 {
		t.Fatalf("%d drops and %d graded overalls in %d cases: the generator is too tame",
			drops, graded, propertyCases)
	}
}
