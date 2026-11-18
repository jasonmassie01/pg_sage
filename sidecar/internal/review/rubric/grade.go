package rubric

import (
	"encoding/json"
	"math"
)

// Status is how one check came out.
type Status string

// Check statuses. warn counts half its weight; unknown counts none but
// stays in the denominator; not_applicable (decided by the platform alone)
// leaves the denominator.
const (
	StatusPass          Status = "pass"
	StatusWarn          Status = "warn"
	StatusFail          Status = "fail"
	StatusUnknown       Status = "unknown"
	StatusNotApplicable Status = "not_applicable"
)

// Letter is a grade, A to F; "" is unknown and marshals as null.
type Letter string

// MarshalJSON writes an unknown letter as null.
func (l Letter) MarshalJSON() ([]byte, error) {
	if l == "" {
		return []byte("null"), nil
	}
	return json.Marshal(string(l))
}

// UnmarshalJSON reads null as unknown.
func (l *Letter) UnmarshalJSON(b []byte) error {
	var s *string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*l = ""
	if s != nil {
		*l = Letter(*s)
	}
	return nil
}

// Grade arithmetic thresholds.
const (
	minCoverage   = 60.0 // below it a dimension's letter is unknown
	minKnown      = 4    // fewer known dimensions leave the overall unknown
	warnFraction  = 0.5
	criticalLimit = Letter("D")
)

// CheckResult is one check's outcome with the evidence it rests on and,
// when unknown or not_applicable, why.
type CheckResult struct {
	Check    string   `json:"check"`
	Status   Status   `json:"status"`
	Weight   int      `json:"weight"`
	Critical bool     `json:"critical,omitempty"`
	Evidence []string `json:"evidence"`
	Reason   string   `json:"reason,omitempty"`
}

// DimensionGrade is one dimension's score (passed weight over applicable
// weight, in percent), coverage (applicable weight that ran) and letter.
type DimensionGrade struct {
	Dimension string        `json:"dimension"`
	Title     string        `json:"title"`
	Letter    Letter        `json:"letter"`
	Score     float64       `json:"score"`
	Coverage  float64       `json:"coverage"`
	Checks    []CheckResult `json:"checks"`
	// applicable is the weight that is not not_applicable; capped is a
	// critical check that did not pass. Neither is stored: they serve
	// Compute's overall.
	applicable int
	ran        float64
	capped     bool
}

// Grade is a rubric's grade of one database.
type Grade struct {
	RubricVersion string           `json:"rubric_version"`
	Overall       Letter           `json:"overall"`
	OverallScore  float64          `json:"overall_score"`
	CoveragePct   float64          `json:"coverage_pct"`
	Known         int              `json:"known_dimensions"`
	Dimensions    []DimensionGrade `json:"dimensions"`
}

var letters = []Letter{"A", "B", "C", "D", "F"}

// LetterFor is the letter of a score: A at 90, B at 80, C at 70, D at 60,
// F below.
func LetterFor(score float64) Letter {
	switch {
	case score >= 90:
		return "A"
	case score >= 80:
		return "B"
	case score >= 70:
		return "C"
	case score >= 60:
		return "D"
	}
	return "F"
}

// worse is the lower of two letters.
func worse(a, b Letter) Letter {
	if letterIndex(a) > letterIndex(b) {
		return a
	}
	return b
}

func letterIndex(l Letter) int {
	for i, x := range letters {
		if x == l {
			return i
		}
	}
	return len(letters)
}

// oneAbove is the letter one better than l (A stays A).
func oneAbove(l Letter) Letter {
	return letters[max(letterIndex(l)-1, 0)]
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Compute grades results against r. A check of r without a result is
// unknown; a result for a check r lacks is ignored.
func Compute(r Rubric, results []CheckResult) Grade {
	byID := make(map[string]CheckResult, len(results))
	for _, res := range results {
		byID[res.Check] = res
	}
	g := Grade{RubricVersion: r.Version}
	for _, d := range r.Dimensions {
		g.Dimensions = append(g.Dimensions, gradeDimension(d, byID))
	}
	overall(&g)
	return g
}

func gradeDimension(d Dimension, byID map[string]CheckResult) DimensionGrade {
	out := DimensionGrade{Dimension: d.ID, Title: d.Title}
	var ran, passed float64
	for _, spec := range d.Checks {
		res, ok := byID[spec.ID]
		if !ok {
			res = CheckResult{Check: spec.ID, Status: StatusUnknown, Reason: "no result"}
		}
		res.Weight, res.Critical = spec.Weight, spec.Critical
		if res.Evidence == nil {
			res.Evidence = []string{}
		}
		out.Checks = append(out.Checks, res)
		if res.Status == StatusNotApplicable {
			continue
		}
		r, p := tally(res.Status, float64(spec.Weight))
		ran, passed = ran+r, passed+p
		out.applicable += spec.Weight
		// A critical check that failed, or could not show it passed, caps
		// the dimension, so an unknown can never lift the cap.
		ok = res.Status == StatusPass || res.Status == StatusWarn
		out.capped = out.capped || spec.Critical && !ok
	}
	out.ran = ran
	if out.applicable == 0 {
		return out
	}
	out.Score = round2(100 * passed / float64(out.applicable))
	out.Coverage = round2(100 * ran / float64(out.applicable))
	if out.Coverage >= minCoverage {
		out.Letter = out.rawLetter()
	}
	return out
}

// tally is the weight a result ran and the weight it passed: unknown (or
// a status this version does not know) ran none and passed none.
func tally(s Status, w float64) (ran, passed float64) {
	switch s {
	case StatusPass:
		return w, w
	case StatusWarn:
		return w, w * warnFraction
	case StatusFail:
		return w, 0
	}
	return 0, 0
}

// rawLetter is the letter of the score with the critical cap, whatever the
// coverage: the overall's cap reads it for every applicable dimension.
func (d DimensionGrade) rawLetter() Letter {
	l := LetterFor(d.Score)
	if d.capped {
		l = worse(l, criticalLimit)
	}
	return l
}

// overall sets the overall score, coverage and letter. Every dimension
// with applicable weight counts, known or not: its unknown checks already
// count nothing, so a check that could not run can only lower the
// overall. The letter is unknown with fewer than four known dimensions,
// and never more than one letter above the worst dimension's letter.
func overall(g *Grade) {
	var sum float64
	var n, applicable int
	var ran float64
	worst := Letter("A")
	for _, d := range g.Dimensions {
		if d.applicable == 0 {
			continue
		}
		n++
		sum += d.Score
		applicable += d.applicable
		ran += d.ran
		worst = worse(worst, d.rawLetter())
		if d.Letter != "" {
			g.Known++
		}
	}
	if n == 0 {
		return
	}
	g.OverallScore = round2(sum / float64(n))
	g.CoveragePct = round2(100 * ran / float64(applicable))
	if g.Known >= minKnown {
		g.Overall = worse(LetterFor(g.OverallScore), oneAbove(worst))
	}
}
